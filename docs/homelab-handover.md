# Handover: wire `k8s-app-standby-toggle` into the homelab

A brief for the agent that maintains the homelab repository. It answers the
handover *"build `k8s-app-standby-toggle`"*: the service, image and chart now
exist and are released. What is left is on the homelab side: the Argo CD
prerequisite, the Authelia client, installing the chart, the CI runner and the
alert silencing. This document is self-contained; the
[README](../README.md) has the full reference.

## Table of Contents

- [What exists](#what-exists)
- [What you need to do](#what-you-need-to-do)
  - [1. Argo CD prerequisite](#1-argo-cd-prerequisite)
  - [2. Authelia client](#2-authelia-client)
  - [3. Namespace, CA and session key](#3-namespace-ca-and-session-key)
  - [4. Install the chart](#4-install-the-chart)
  - [5. CI runner](#5-ci-runner)
  - [6. Silence probe alerts for sleeping apps](#6-silence-probe-alerts-for-sleeping-apps)
- [Contracts you can rely on](#contracts-you-can-rely-on)
- [Differences from the original handover](#differences-from-the-original-handover)
- [Acceptance criteria](#acceptance-criteria)
- [Smoke test after deploying](#smoke-test-after-deploying)
- [Known limitations](#known-limitations)

## What exists

| What | Where |
| --- | --- |
| Repository | `github.com/brnl/k8s-app-standby-toggle` (public) |
| Chart | `oci://ghcr.io/brnl/charts/k8s-app-standby-toggle`, version `0.1.0` (public, cosign-signed) |
| Image | `ghcr.io/brnl/k8s-app-standby-toggle:0.1.0` (amd64, arm64, armv7; cosign-signed; SBOM and provenance) |
| Release notes | GitHub release `v0.1.0` |

The chart's image tag follows `appVersion`, which equals the chart `version`:
Renovate bumping the chart version is also the image bump. The image digest
is not pinned in the chart; set `image.digest` if you want that.

Confirmed with the owner before building: annotation prefix
`k8s-app-standby-toggle.cloud64.net/`; wake for 1h, 4h or 24h; hold for 24h
(default and maximum).

## What you need to do

### 1. Argo CD prerequisite

Without this, Argo CD's self-heal scales every sleeping Deployment straight
back up; the service then marks the group `conflict` after 3 undone sleeps in
10 minutes and stops acting on it (it does not flap).

Add to the ApplicationSet template(s) that generate the Applications in
groups that can sleep (adding it to all of them is harmless):

```yaml
spec:
  ignoreDifferences:
    - group: apps
      kind: Deployment
      jsonPointers: [/spec/replicas]
    - group: apps
      kind: StatefulSet
      jsonPointers: [/spec/replicas]
  syncPolicy:
    syncOptions:
      - RespectIgnoreDifferences=true
```

Two more entries are needed **only if** an app's manifests set these fields
explicitly (Argo CD ignores fields its manifests do not set):

```yaml
    - group: batch
      kind: CronJob
      jsonPointers: [/spec/suspend]
    - group: postgresql.cnpg.io
      kind: Cluster
      jsonPointers: [/metadata/annotations/cnpg.io~1hibernation]
```

The service also adds its own annotations
(`k8s-app-standby-toggle.cloud64.net/replicas`, `/suspend`, `/hibernation`)
to the objects it puts to sleep. Argo CD does not diff annotations its
manifests do not set, so nothing is needed for those.

### 2. Authelia client

The service is a **public** client with PKCE (S256): there is no client
secret. Groups are read from the userinfo endpoint, so the `groups` scope is
required but no ID-token claims policy is.

```yaml
identity_providers:
  oidc:
    clients:
      - client_id: k8s-app-standby-toggle
        client_name: Standby toggle
        public: true
        authorization_policy: two_factor   # or one_factor
        require_pkce: true
        pkce_challenge_method: S256
        redirect_uris:
          - https://<standby hostname>/auth/callback
        scopes: [openid, profile, groups]
        response_types: [code]
        grant_types: [authorization_code]
        token_endpoint_auth_method: none
        userinfo_signed_response_alg: none
```

The username shown in the UI and recorded on overrides and holds is
`preferred_username`.

### 3. Namespace, CA and session key

- **Namespace.** Any namespace; Pod Security `baseline` (the cluster default)
  is fine, the pod also passes `restricted`. The service never touches its
  own namespace, `kube-system`, `kube-public`, `kube-node-lease` or the Argo
  CD namespace.
- **Issuer CA.** Authelia's certificate is self-signed here, so put its CA
  (PEM) in a ConfigMap or Secret in the service's namespace and set
  `oidc.ca.configMap` (or `oidc.ca.secret`) and `oidc.ca.key` (default
  `ca.crt`). The service trusts the system roots plus that file for the issuer
  only.
- **Session key (recommended).** A Secret with at least 32 random bytes, for
  example `openssl rand -base64 48`, referenced by
  `oidc.sessionKeySecret.name` (key `session-key`). Without it every restart,
  including each chart upgrade, signs everyone out (acceptable, but noisy).
- **Network.** If you use NetworkPolicies, the pod needs egress to the
  Kubernetes API server and to Authelia (the issuer), and ingress from the
  gateway (UI and API) and Prometheus (metrics on port 8080).

### 4. Install the chart

Add an Application (or an entry in the matching ApplicationSet) for the
chart. Example values; the `groups:` key is exactly the shape the pipeline
reads:

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
  issuerURL: https://<authelia hostname>
  allowedGroups: [admins]          # Authelia groups that may use the UI
  ca:
    configMap: authelia-ca          # from step 3
  sessionKeySecret:
    name: k8s-app-standby-toggle-session

machineAuth:
  subjects:
    - system:serviceaccount:ci-runner:ci-runner

httpRoute:
  enabled: true
  parentRefs:
    - name: <gateway>
      namespace: <gateway namespace>
      sectionName: <https listener>
  hostnames:
    - <standby hostname>           # oidc.redirectURL defaults to https://<this>/auth/callback

metrics:
  serviceMonitor:
    enabled: true                   # no release label needed on this cluster
```

Notes:

- The chart fails to render (so your CI catches it) for an unknown key, an
  Application in two groups, an empty group, a `default` other than
  `always-on`/`awake`/`asleep`, a missing issuer, or no allowed groups (set
  `oidc.allowAnyUser: true` to allow every authenticated user instead).
- Never use `on`/`off` for `default`: YAML reads them as booleans, and the
  schema rejects them.
- A change to `groups` restarts the pod (checksum annotation). Runtime state
  (overrides, leases, holds) survives: it lives in the ConfigMap
  `<release>-k8s-app-standby-toggle-state`, created by the service. **Do not
  manage that ConfigMap from Git**; Argo CD does not track it because the
  service creates it without tracking metadata.
- Do not put the standby service's own Application in a group that can sleep
  (it would never wake itself). It skips its own namespace anyway.
- The HTTPRoute routes only `/`, `/api/`, `/auth/`, `/ui/` and `/static/`.
  `/metrics`, `/healthz` and `/readyz` are not exposed through the gateway.
- One replica, `Recreate` strategy. Requests 10m CPU / 32Mi, limit 96Mi.

### 5. CI runner

The pipeline authenticates with its ServiceAccount token for the audience
`k8s-app-standby-toggle`, checked with a TokenReview. Mount a projected token
in the runner (or job) pod:

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

A machine may read groups, create/renew/delete leases and set a hold (it
cannot shorten an existing hold). It may not set or delete overrides, lift a
hold or clear a conflict (`403`).

The flow, as in the original handover:

```sh
API=https://<standby hostname>/api/v1        # or http://<svc>.<ns>.svc:8080/api/v1 inside the cluster
TOKEN=$(cat /var/run/secrets/standby/token)

curl -fsS -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"ttl":"45m"}' "$API/groups/vikunja/leases/ci-run-$RUN_ID"
until curl -fsS -H "Authorization: Bearer $TOKEN" "$API/groups/vikunja" \
      | jq -e '.phase == "awake" and .ready' >/dev/null; do sleep 5; done
# run tests; on failure:
curl -fsS -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"reason\":\"$RUN_URL\"}" "$API/groups/vikunja/hold"
# always:
curl -fsS -X DELETE -H "Authorization: Bearer $TOKEN" "$API/groups/vikunja/leases/ci-run-$RUN_ID"
```

- Lease owners: letters, digits, `.`, `_`, `-`, at most 128 characters.
- Lease `ttl` ≤ 4h (`lease.maxDuration`); hold `ttl` ≤ 24h, default 24h.
- Put a timeout on the polling loop: a group that never gets ready reports
  `phase: degraded` after 10 minutes (`degradedAfter`), and a group in
  `conflict` never becomes ready.
- From inside the cluster the Service is reachable directly; the API is
  served on the same port as the UI.

### 6. Silence probe alerts for sleeping apps

Stable metrics (names and labels will not change without a major version):

| Metric | Labels | Value |
| --- | --- | --- |
| `k8s_app_standby_toggle_group_should_be_awake` | `group` | 1 awake, 0 asleep |
| `k8s_app_standby_toggle_application_should_be_awake` | `group`, `application` | the same, one series per Application |
| `k8s_app_standby_toggle_group_ready` | `group` | 1 if it should be awake and is ready |
| `k8s_app_standby_toggle_group_held` | `group` | 1 if held |
| `k8s_app_standby_toggle_group_conflict` | `group` | 1 if the service stopped acting on it |
| `k8s_app_standby_toggle_group_phase` | `group`, `phase` | 1 for the current phase |
| `k8s_app_standby_toggle_application_missing` | `group`, `application` | 1 if the Application does not exist |
| `k8s_app_standby_toggle_reconcile_errors_total` | | counter |
| `k8s_app_standby_toggle_build_info` | `version`, `goversion` | 1 |

`always-on` groups report 1. The per-group series are absent for a few
seconds after a restart, until the stored state has been read, rather than
reporting a wrong value. Example for a probe alert that carries the Argo CD
Application name in a label `application`:

```promql
<probe alert expression>
  unless on (application)
  (k8s_app_standby_toggle_application_should_be_awake == 0)
```

Useful alerts of its own: `k8s_app_standby_toggle_group_conflict == 1`,
`k8s_app_standby_toggle_group_phase{phase="degraded"} == 1`, and
`k8s_app_standby_toggle_kubernetes_api_reachable == 0`.

## Contracts you can rely on

- **Values:** `groups:` is a map of group name (a-z, 0-9, `-`) to
  `default` and `apps`. Nothing else is read from a group.
- **API:** as specified in the original handover, under `/api/v1`, with the
  same status codes. Errors are `{"error": "..."}`.
- **Metrics:** the table above.
- **Annotations on workloads:** `k8s-app-standby-toggle.cloud64.net/replicas`
  (Deployment, StatefulSet), `/suspend` (CronJob), `/hibernation` (CNPG
  Cluster). Present only while the service has the object asleep. To take an
  object out of the service's hands by hand, restore the value and remove the
  annotation.

## Differences from the original handover

Deliberate, small, and documented in the README:

| Topic | Original | Built | Why |
| --- | --- | --- | --- |
| RBAC | `patch` on the `scale` subresource | not granted | The service sets `spec.replicas` and the recorded-value annotation in one conditional merge patch on the object, so `scale` would be unused. |
| CNPG wake | set `cnpg.io/hibernation: "off"` | `"off"`, unless the cluster was already `"on"` before the service hibernated it | Same "restore the previous value" rule as replicas. |
| API | 8 endpoints | + `DELETE /groups/{group}/conflict` (people only) and `GET /whoami` | A conflict needs a way to be cleared; `whoami` helps debug tokens. |
| Group JSON | example fields | + `source`, `conflict`, `missing`, `errors`, `observed`, and `application`/`state` per resource | Same fields otherwise; consumers can ignore the extras. |
| `GET /groups` | "all groups" | a JSON array of groups | |
| Override `ttl` | required for awake | must be one of the wake durations (1h, 4h, 24h); optional for asleep | "allowed durations". |
| Machine holds | may set a hold | may set or replace one, but never shorten it | Otherwise a machine could lift a hold by replacing it with a short one. |
| Lease maximum | configurable | default 4h | |
| Readiness probe | — | uses `/healthz`; `/readyz` reflects API reachability | Keeps the UI reachable (with the error shown) while the API is down. |
| Metrics | per group | + per-Application `should_be_awake` and `missing` | Easier to join with per-app probe alerts. |
| Phase | 5 phases + conflict | + `unknown` before the first observation | |

## Acceptance criteria

| Criterion | Verified by |
| --- | --- |
| `default: asleep` group asleep after startup without a call | e2e on kind; unit test |
| Wake via UI for 1h restores recorded replicas, sleeps again after the hour | unit test (fake clock); e2e covers restore |
| Lease wakes; delete sleeps; never-deleted lease expires | e2e; unit tests |
| Hold keeps the group awake after the lease, reason shown in the UI | e2e; UI test |
| CNPG cluster hibernated on sleep, back with its data on wake | unit tests against a fake API (annotation, order, readiness). **Needs a real check on the cluster**: CNPG is not installed in the e2e cluster. |
| Deployment added to a sleeping Application asleep within one reconcile | e2e; unit test |
| `always-on` cannot be changed through UI or API | API tests (`409`), UI test (no controls), e2e |
| `401` unauthenticated; `403` outside allowed groups / subjects | e2e (tokens); OIDC flow test against a fake issuer. **Needs a real check with Authelia.** |
| Without the Argo CD prerequisite: `conflict`, service stops acting | e2e (simulated self-heal); unit test |
| Non-root, read-only root filesystem, Pod Security | e2e enforces `restricted` on the namespace |
| CI green, signed image and public chart | see the PR and the `v0.1.0` release |

## Smoke test after deploying

1. `kubectl -n <ns> logs deploy/<release>-k8s-app-standby-toggle` shows
   `starting k8s-app-standby-toggle` with your groups, then
   `group state` per group, no `reconcile failed`.
2. The UI at `https://<standby hostname>/` redirects to Authelia and back;
   your username shows top right. A user outside `allowedGroups` gets
   "Access denied".
3. Groups with `default: asleep` show `asleep`; their Deployments carry
   `k8s-app-standby-toggle.cloud64.net/replicas`.
4. Wake one for 1h from the UI; it reaches `awake`; its CNPG cluster (if
   any) leaves hibernation before the app starts, and the app's data is
   there.
5. No group shows `conflict`. If one does, the Argo CD prerequisite is
   missing for that app.
6. From the runner: `curl -H "Authorization: Bearer $TOKEN" $API/whoami`
   returns the ServiceAccount; a lease on an `always-on` group returns `409`.
7. Prometheus has `k8s_app_standby_toggle_group_should_be_awake` for every
   group.

## Known limitations

- CronJobs are suspended, but a Job that is already running is not stopped.
- Workloads managed by an HPA, or owned by another controller, are not
  supported: owned objects are skipped; an HPA fighting the service ends in
  `conflict`.
- Single replica, single cluster, no leader election, development clusters
  only (as specified).
- If GHCR ever reports a new package as private, make it public once in the
  package settings; the release workflow warns when the chart or image
  cannot be pulled anonymously.
