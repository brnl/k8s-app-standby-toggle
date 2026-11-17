# k8s-app-standby-toggle

A small service for a **development** Kubernetes cluster that puts groups of
Argo CD Applications to sleep (scaled to zero) and wakes them again, to save
memory without removing apps from the cluster.

- **People** use a web page: wake a group for 1h, 4h or 24h to try something,
  put one to sleep, or hold a broken one awake to investigate.
- **CI pipelines** use an HTTP API: take a lease on a group, wait until it is
  ready, run tests, release it (or hold it when the tests fail).
- **The service itself** puts a woken group back to sleep when its time is up
  ("auto-snooze"). Supporting services are marked `always-on` and are never
  touched.

It is one static Go binary (no client-go; only `go-oidc` and `oauth2` for
login) on a distroless image, and a Helm chart. Images are multi-arch (amd64,
arm64, armv7), signed with cosign, and published with an SBOM and provenance.

## Table of contents

- [How it works](#how-it-works)
- [Prerequisite on the Argo CD side](#prerequisite-on-the-argo-cd-side)
- [Install](#install)
- [Configuration](#configuration)
- [Authentication](#authentication)
- [HTTP API](#http-api)
- [Web UI](#web-ui)
- [Metrics and health](#metrics-and-health)
- [Security](#security)
- [Development](#development)
- [Releasing](#releasing)

## How it works

### Groups

A **group** is a named set of Argo CD Applications that sleep and wake
together, with a default state. Groups come from the chart values and never
change at runtime.

| `default` | Meaning |
| --- | --- |
| `always-on` | Never sleeps. Shown without controls; the API refuses changes (`409`). |
| `awake` | Runs unless someone puts it to sleep. |
| `asleep` | Sleeps unless something wakes it. |

### Deviations from the default

| Kind | Set by | Effect | Expiry |
| --- | --- | --- | --- |
| **Override** | a person | `awake` or `asleep` | `awake` always expires (auto-snooze); `asleep` may be open-ended. One per group. |
| **Lease** | a machine (or a person) | keeps the group awake while a pipeline works | mandatory; several per group, one per owner |
| **Hold** | a person or a machine | keeps the group awake for investigation, with a reason | mandatory (default and maximum 24h). One per group. |

The **effective state** is the first rule that matches:

1. `default: always-on` → awake
2. an unexpired hold → awake
3. an unexpired lease → awake
4. an unexpired override → the override's state
5. the default

Expired items are removed at the next reconcile. Auto-snooze is time-based
only; there is no idle or traffic detection.

### Sleeping and waking

The service reads `status.resources` of each Application in a group and acts on
the kinds below. Everything else (Services, DaemonSets, HPAs, ...) is skipped.

| Kind | Sleep | Wake |
| --- | --- | --- |
| `apps/Deployment`, `apps/StatefulSet` | record `spec.replicas` in the annotation `k8s-app-standby-toggle.cloud64.net/replicas`, then scale to 0 | restore `spec.replicas`, remove the annotation |
| `postgresql.cnpg.io/Cluster` (CloudNativePG) | record `cnpg.io/hibernation` in `k8s-app-standby-toggle.cloud64.net/hibernation`, set `cnpg.io/hibernation: "on"` | set `cnpg.io/hibernation: "off"` (or back to `"on"` if it was hibernated before), remove the record |
| `batch/CronJob` | record `spec.suspend` in `k8s-app-standby-toggle.cloud64.net/suspend`, set it to `true` | restore it |

- The previous value is always recorded, including 0: a Deployment at 0
  before sleeping is at 0 after waking. An object without a recorded value is
  left alone on wake. Recording and scaling happen in one patch that only
  applies if the object did not change since it was read.
- **Order.** On wake, CloudNativePG clusters first; the rest once they are
  ready (all instances ready, not hibernated), or after `databaseWait` (5m)
  at the latest. On sleep, the rest first; the clusters once those pods are
  gone (or after `databaseWait`). Hibernation keeps the PVCs, so the data
  comes back on wake.
- **Skipped:** objects with `ownerReferences` (owned by another controller),
  DaemonSets, and anything in `kube-system`, `kube-public`,
  `kube-node-lease`, the Argo CD namespace or the service's own namespace,
  whatever the configuration says. Each skip is logged once.
- **Ready** means: for a Deployment or StatefulSet, the controller observed
  the current generation and `status.readyReplicas` equals `spec.replicas`;
  for a CloudNativePG cluster, phase `Cluster in healthy state`, no
  hibernation condition, and `status.readyInstances` equals `spec.instances`.

### Reconcile loop and phases

Every 30 seconds (every 5 seconds while a group is changing), and right after
every API call that changes state, the service drops expired items, computes
each group's effective state and brings every resource to it. Every action is
idempotent, so a resource added to a sleeping Application (for example by a
chart upgrade) is put to sleep at the next reconcile.

| Phase | Meaning |
| --- | --- |
| `awake` | should be awake; every resource is woken and ready (`ready: true`) |
| `waking` | should be awake; not ready yet |
| `degraded` | should be awake; still not ready after `degradedAfter` (10m) |
| `sleeping` | should be asleep; pods are still going away |
| `asleep` | should be asleep; every resource is asleep |
| `conflict` | the service stopped acting on the group (see below) |
| `unknown` | not observed yet (right after startup) |

**Conflict.** If resources the service put to sleep are found awake again
`conflict.threshold` (3) times within `conflict.window` (10m), something else
is fighting it: Argo CD without the prerequisite below, or an autoscaler. The
service then stops acting on that group, marks it `conflict` with the reason
(UI, API and the `k8s_app_standby_toggle_group_conflict` metric) and does not
flap. Fix the cause, then press *Clear conflict and retry* (or
`DELETE /api/v1/groups/{group}/conflict`).

### State

Overrides, leases, holds and conflicts are stored in one ConfigMap in the
service's namespace, `<release>-k8s-app-standby-toggle-state`. The service
creates it at runtime and updates it with optimistic concurrency; the chart
deliberately does not create it, so Argo CD never resets it. The service runs
as a single replica without leader election.

## Prerequisite on the Argo CD side

With automated sync and self-heal, Argo CD would scale every sleeping
Deployment straight back up. The Applications of the managed groups must
ignore the replica count, and syncs must respect that:

```yaml
# In the Application (or ApplicationSet template) spec
spec:
  ignoreDifferences:
    - group: apps
      kind: Deployment
      jsonPointers: [/spec/replicas]
    - group: apps
      kind: StatefulSet
      jsonPointers: [/spec/replicas]
    # Only needed when the manifests set these fields explicitly:
    - group: batch
      kind: CronJob
      jsonPointers: [/spec/suspend]
    - group: postgresql.cnpg.io
      kind: Cluster
      jsonPointers: [/metadata/annotations/cnpg.io~1hibernation]
  syncPolicy:
    syncOptions:
      - RespectIgnoreDifferences=true
```

Without it, the group ends in `conflict` within a few reconciles and the
service leaves it alone.

## Install

The chart is published as a public OCI artifact; its version is also the
image tag.

```sh
helm install standby oci://ghcr.io/brnl/charts/k8s-app-standby-toggle \
  --version 0.1.0 --namespace standby --create-namespace -f values.yaml
```

A minimal `values.yaml`:

```yaml
groups:
  identity:
    default: always-on
    apps: [lldap, authelia-infra, authelia, authelia-valkey]
  vikunja:
    default: awake
    apps: [vikunja, vikunja-infra]
  nextcloud:
    default: asleep
    apps: [nextcloud, nextcloud-infra, nextcloud-valkey]
oidc:
  issuerURL: https://auth.example.com
  allowedGroups: [admins]
  ca:
    configMap: authelia-ca # only for a self-signed issuer
machineAuth:
  subjects:
    - system:serviceaccount:ci-runner:ci-runner
httpRoute:
  enabled: true
  parentRefs:
    - name: gateway
      namespace: gateway-system
  hostnames:
    - standby.example.com
metrics:
  serviceMonitor:
    enabled: true
```

## Configuration

### Chart values

The values are validated against a strict schema (a typo fails the install),
and the chart also rejects an Application listed in two groups, an empty
group and an unknown `default` at render time. See
[`values.yaml`](charts/k8s-app-standby-toggle/values.yaml) for every value.

| Value | Default | Description |
| --- | --- | --- |
| `groups` | `{}` | Group name → `default` and `apps` (required). **This shape is a contract**: other tooling reads the same key. |
| `argocd.namespace` | `argocd` | Namespace of the Applications |
| `reconcileInterval` | `30s` | Reconcile period |
| `wakeDurations` | `[1h, 4h, 24h]` | The durations a person can wake a group for |
| `hold.defaultDuration` / `hold.maxDuration` | `24h` / `24h` | Hold duration when none is given / at most |
| `lease.maxDuration` | `4h` | Longest lease |
| `degradedAfter` | `10m` | Waking longer than this is `degraded` |
| `databaseWait` | `5m` | Bound on waiting for CloudNativePG clusters (wake) or for pods to go (sleep) |
| `conflict.threshold` / `conflict.window` | `3` / `10m` | When an undone sleep becomes a conflict |
| `annotationPrefix` | `k8s-app-standby-toggle.cloud64.net/` | Prefix of the recorded-value annotations |
| `oidc.issuerURL` | — | OIDC issuer (required) |
| `oidc.clientID` | `k8s-app-standby-toggle` | Public client ID |
| `oidc.redirectURL` | `https://<httpRoute.hostnames[0]>/auth/callback` | Registered redirect URL |
| `oidc.allowedGroups` | `[]` | Groups that may sign in (required unless `oidc.allowAnyUser`) |
| `oidc.ca.configMap` / `oidc.ca.secret` / `oidc.ca.key` | — / — / `ca.crt` | CA of a self-signed issuer |
| `oidc.sessionTTL` | `12h` | Sign-in lifetime |
| `oidc.sessionKeySecret.name` / `.key` | — / `session-key` | Secret with the cookie signing key (≥ 32 bytes); without it a restart signs everyone out |
| `machineAuth.audience` | `k8s-app-standby-toggle` | Audience of machine tokens |
| `machineAuth.subjects` | `[]` | ServiceAccounts that may use the API |
| `httpRoute.enabled` / `parentRefs` / `hostnames` | `false` | Gateway API HTTPRoute for `/`, `/api/`, `/auth/`, `/ui/`, `/static/` (not `/metrics` or the health endpoints) |
| `metrics.serviceMonitor.enabled` | `false` | Prometheus Operator ServiceMonitor |
| `resources` | 10m CPU, 32Mi/96Mi | Container resources |

A change to `groups` restarts the pod (checksum annotation).

### Environment variables

The binary is configured with environment variables; the chart sets them.

| Variable | Default | Description |
| --- | --- | --- |
| `STANDBY_GROUPS_FILE` | `/etc/k8s-app-standby-toggle/groups/groups.json` | JSON object: group → `{"default": ..., "apps": [...]}` |
| `STANDBY_NAMESPACE` | the pod's namespace | Own namespace (state ConfigMap; never touched) |
| `STANDBY_ARGOCD_NAMESPACE` | `argocd` | Namespace of the Applications |
| `STANDBY_RECONCILE_INTERVAL` | `30s` | |
| `STANDBY_WAKE_DURATIONS` | `1h,4h,24h` | |
| `STANDBY_HOLD_DEFAULT` / `STANDBY_HOLD_MAX` | `24h` / `24h` | |
| `STANDBY_LEASE_MAX` | `4h` | |
| `STANDBY_DEGRADED_AFTER` | `10m` | |
| `STANDBY_DATABASE_WAIT` | `5m` | |
| `STANDBY_CONFLICT_THRESHOLD` / `STANDBY_CONFLICT_WINDOW` | `3` / `10m` | |
| `STANDBY_ANNOTATION_PREFIX` | `k8s-app-standby-toggle.cloud64.net/` | |
| `STANDBY_STATE_CONFIGMAP` | `k8s-app-standby-toggle-state` | |
| `STANDBY_LISTEN_ADDR` | `:8080` | |
| `STANDBY_KUBE_API_URL` | in-cluster | API server URL for development (e.g. `http://127.0.0.1:8001` with `kubectl proxy`) |
| `STANDBY_AUTH_DISABLED` | `false` | Development only; refuses to start unless the listener is on localhost |
| `STANDBY_OIDC_ISSUER_URL`, `_CLIENT_ID`, `_REDIRECT_URL`, `_CA_FILE` | | See above |
| `STANDBY_OIDC_ALLOWED_GROUPS` / `STANDBY_OIDC_ALLOW_ANY_USER` | — / `false` | |
| `STANDBY_SESSION_KEY_FILE` / `STANDBY_SESSION_TTL` | — / `12h` | |
| `STANDBY_MACHINE_AUDIENCE` / `STANDBY_MACHINE_SUBJECTS` | `k8s-app-standby-toggle` / — | |
| `STANDBY_LOG_LEVEL` / `STANDBY_LOG_FORMAT` | `info` / `json` | |

Startup fails with a clear message for an Application in two groups, an
unknown `default`, an empty group, no allowed groups (without
`STANDBY_OIDC_ALLOW_ANY_USER`), or disabled authentication on a non-local
listener. An Application that does not exist in the cluster is not an error:
it is shown as missing.

## Authentication

One listener. A request with `Authorization: Bearer` is a machine; anything
else is a browser session.

### People: OpenID Connect

The service is its own relying party, a **public client with PKCE (S256)**, so
there is no client secret. It uses the authorization code flow with `state`
and `nonce`, scopes `openid profile groups`, and reads group membership from
the **userinfo** endpoint (Authelia does not put `groups` in the ID token by
default). A user outside `oidc.allowedGroups` gets `403`. The session is a
signed `Secure`, `HttpOnly`, `SameSite=Lax` cookie; state-changing browser
requests need the session's CSRF token and pass Go's cross-origin protection.
The username is recorded on every override and hold.

An Authelia client for it:

```yaml
identity_providers:
  oidc:
    clients:
      - client_id: k8s-app-standby-toggle
        client_name: Standby toggle
        public: true
        authorization_policy: two_factor
        require_pkce: true
        pkce_challenge_method: S256
        redirect_uris:
          - https://standby.example.com/auth/callback
        scopes: [openid, profile, groups]
        response_types: [code]
        grant_types: [authorization_code]
        token_endpoint_auth_method: none
        userinfo_signed_response_alg: none
```

### Machines: ServiceAccount tokens

Bearer tokens are checked with a `TokenReview` for the audience
`k8s-app-standby-toggle`, and only subjects in `machineAuth.subjects` are
accepted (results are cached for 30s). A machine may read groups, create,
renew and delete leases, and set a hold (it cannot shorten an existing one).
It may not set or delete overrides, lift a hold or clear a conflict.

Give the pipeline a token for that audience, for example a projected volume:

```yaml
volumes:
  - name: standby-token
    projected:
      sources:
        - serviceAccountToken:
            audience: k8s-app-standby-toggle
            expirationSeconds: 3600
            path: token
```

or, for a quick test, `kubectl -n ci-runner create token ci-runner --audience k8s-app-standby-toggle`.

## HTTP API

JSON under `/api/v1`. Durations are Go duration strings (`"45m"`).

| Method and path | Body | Who | Effect |
| --- | --- | --- | --- |
| `GET /groups` | — | all | All groups (a JSON array) |
| `GET /groups/{group}` | — | all | One group |
| `PUT /groups/{group}/override` | `{"state":"awake","ttl":"4h"}` | people | Set or replace the override. `ttl` is required for `awake` and must be one of the wake durations; optional for `asleep`. |
| `DELETE /groups/{group}/override` | — | people | Back to the default |
| `PUT /groups/{group}/leases/{owner}` | `{"ttl":"45m"}` | all | Create or renew a lease (≤ `lease.maxDuration`) |
| `DELETE /groups/{group}/leases/{owner}` | — | all | Release it |
| `PUT /groups/{group}/hold` | `{"reason":"...","ttl":"24h"}` | all | Set or replace the hold; `ttl` optional (default 24h) |
| `DELETE /groups/{group}/hold` | — | people | Lift it |
| `DELETE /groups/{group}/conflict` | — | people | Clear a conflict; the service acts on the group again |
| `GET /whoami` | — | all | The authenticated caller |

Every call that changes state returns the group. Deleting something that does
not exist is not an error. Errors are `{"error": "..."}` with `400` (invalid
body or duration), `401` (not authenticated), `403` (not allowed), `404`
(unknown group) or `409` (the group is `always-on`).

A group:

```json
{
  "name": "vikunja",
  "apps": ["vikunja", "vikunja-infra"],
  "default": "asleep",
  "desired": "awake",
  "phase": "waking",
  "ready": false,
  "reason": "lease ci-run-123 until 2026-11-16T14:05:00Z",
  "source": "lease",
  "override": null,
  "leases": [{"owner": "ci-run-123", "by": "system:serviceaccount:ci-runner:ci-runner", "expires": "2026-11-16T14:05:00Z", "created": "2026-11-16T13:20:00Z"}],
  "hold": null,
  "conflict": null,
  "missing": [],
  "resources": [{"application": "vikunja", "kind": "Deployment", "namespace": "vikunja", "name": "vikunja", "replicas": 1, "ready": 0, "state": "changing"}],
  "observed": "2026-11-16T13:20:01Z"
}
```

### A CI pipeline

```sh
API=https://standby.example.com/api/v1
TOKEN=$(cat /var/run/secrets/standby/token)
RUN=ci-run-$CI_RUN_ID

# 1. Take a lease (renew it the same way for long runs).
curl -fsS -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"ttl":"45m"}' "$API/groups/vikunja/leases/$RUN"

# 2. Wait until the group is awake and ready.
until curl -fsS -H "Authorization: Bearer $TOKEN" "$API/groups/vikunja" \
      | jq -e '.phase == "awake" and .ready' >/dev/null; do sleep 5; done

# 3. Run the tests.
if ! ./run-tests.sh; then
  # 4a. Failed: keep it awake for investigation.
  curl -fsS -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
    -d "{\"reason\":\"tests failed: $CI_RUN_URL\"}" "$API/groups/vikunja/hold"
fi
# 4b. Always release the lease.
curl -fsS -X DELETE -H "Authorization: Bearer $TOKEN" "$API/groups/vikunja/leases/$RUN"
```

A run that crashes cannot keep the group awake past its lease.

## Web UI

`/` shows one row per group: its Applications (missing ones marked), the
default, the phase and the reason in words ("asleep by default", "awake until
14:05, woken by bram", "held: test failed, run 123"), and controls to wake
for each allowed duration, sleep, hold with a reason, lift the hold, release a
lease, go back to the default and clear a conflict. `always-on` groups have no
controls. The page refreshes itself every 10 seconds. It is server-rendered
HTML with a little plain JavaScript, embedded in the binary.

## Metrics and health

- `/healthz`: the reconcile loop is running. `/readyz`: the last reconcile
  reached the Kubernetes API. The chart's readiness probe uses `/healthz`, so
  the UI stays reachable (and shows the error) while the API is unreachable.
- `/metrics` in Prometheus format. These names and labels are **stable**:

| Metric | Labels | Meaning |
| --- | --- | --- |
| `k8s_app_standby_toggle_group_should_be_awake` | `group` | 1 if the effective state is awake |
| `k8s_app_standby_toggle_application_should_be_awake` | `group`, `application` | the same per Application, to join with per-app alerts |
| `k8s_app_standby_toggle_group_ready` | `group` | 1 if it should be awake and every resource is ready |
| `k8s_app_standby_toggle_group_held` | `group` | 1 if a hold is active |
| `k8s_app_standby_toggle_group_conflict` | `group` | 1 if the service stopped acting on the group |
| `k8s_app_standby_toggle_group_leases` | `group` | number of active leases |
| `k8s_app_standby_toggle_group_phase` | `group`, `phase` | 1 for the current phase |
| `k8s_app_standby_toggle_application_missing` | `group`, `application` | 1 if the Application does not exist |
| `k8s_app_standby_toggle_reconciles_total` | | reconcile passes |
| `k8s_app_standby_toggle_reconcile_errors_total` | | errors during reconciles |
| `k8s_app_standby_toggle_last_reconcile_timestamp_seconds` | | end of the last pass |
| `k8s_app_standby_toggle_kubernetes_api_reachable` | | 1 if the last pass reached the API |
| `k8s_app_standby_toggle_build_info` | `version`, `goversion` | always 1 |

The per-group series appear only once the stored state has been read after a
start, so a restart never reports a wrong "should be awake". To silence probe
alerts for sleeping apps:

```promql
your_probe_alert_expr
  unless on (application) (k8s_app_standby_toggle_application_should_be_awake == 0)
```

Logs are JSON on stdout. Every state change is logged with who caused it
(`by`: a username, a ServiceAccount, `expiry` or `controller`).

## Security

- **RBAC.** `get`, `list`, `watch` on Applications; `get`, `list`, `patch` on
  Deployments, StatefulSets, CronJobs and CloudNativePG Clusters; `create` on
  TokenReviews; in its own namespace `create` on ConfigMaps and `get`,
  `update` on the state ConfigMap only. No `delete`, no pods, no Secrets.
- **Pod.** Non-root (65532), read-only root filesystem, all capabilities
  dropped, no privilege escalation, `RuntimeDefault` seccomp: it passes the
  `restricted` Pod Security Standard (the e2e test enforces it).
- **Web.** Strict Content Security Policy (scripts and styles from the
  service only), no framing, `no-store`, CSRF tokens plus cross-origin
  protection on state-changing browser requests.
- Use on a production cluster is out of scope.

## Development

```sh
go test -race ./...
helm lint --strict charts/k8s-app-standby-toggle -f charts/k8s-app-standby-toggle/ci/default-values.yaml
```

Run against a cluster with `kubectl proxy` and authentication disabled (only
allowed on a localhost listener):

```sh
kubectl proxy &
echo '{"demo":{"default":"asleep","apps":["demo"]}}' > /tmp/groups.json
STANDBY_KUBE_API_URL=http://127.0.0.1:8001 STANDBY_NAMESPACE=default \
STANDBY_GROUPS_FILE=/tmp/groups.json STANDBY_AUTH_DISABLED=true \
STANDBY_LISTEN_ADDR=127.0.0.1:8080 STANDBY_LOG_FORMAT=text \
go run ./cmd/k8s-app-standby-toggle
```

The end-to-end test (`test/e2e/run.sh`, run by CI on kind) installs the chart
next to a hand-made Argo CD Application with a Deployment, a StatefulSet and a
CronJob, and checks sleep on startup, leases, holds, expiry, new resources,
conflict detection, authentication and the Pod Security Standard.

Layout: `cmd/` (main), `internal/config` (environment and groups),
`internal/kube` (minimal API client and a fake API server for tests),
`internal/state` (overrides, leases, holds, effective state, ConfigMap
store), `internal/standby` (sleep and wake per kind), `internal/controller`
(reconcile loop), `internal/auth` (OIDC, sessions, TokenReview),
`internal/web` (API, UI, metrics), `charts/`, `hack/`, `test/e2e/`.

## Releasing

`charts/k8s-app-standby-toggle/Chart.yaml` is the single source of the
version: `appVersion` must equal `version`, and the chart's image tag follows
`appVersion`, so one chart bump is also the image bump.

1. Bump `version` and `appVersion` in `Chart.yaml` in a pull request.
2. Merge it. The release workflow sees a version without a `vX.Y.Z` tag and
   publishes `ghcr.io/brnl/k8s-app-standby-toggle:X.Y.Z` (multi-arch, signed
   with cosign, SBOM and provenance) and the chart
   `oci://ghcr.io/brnl/charts/k8s-app-standby-toggle` version `X.Y.Z`
   (signed), tags the commit and creates a GitHub release. Every push to
   `main` also publishes an `edge` image.

Verify a release:

```sh
cosign verify ghcr.io/brnl/k8s-app-standby-toggle:0.1.0 \
  --certificate-identity-regexp 'https://github.com/brnl/k8s-app-standby-toggle/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```
