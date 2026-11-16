#!/usr/bin/env bash
# End-to-end test on a kind cluster. It installs the chart (image
# $IMAGE, already loaded into the cluster) next to a hand-made Argo CD
# Application with a Deployment, a StatefulSet and a CronJob, and drives the
# service through its API with ServiceAccount tokens.
#
# Requires: kubectl, helm, curl, jq, and the Argo CD Application CRD in
# $ARGOCD_CRD (a file or URL).
set -euo pipefail

IMAGE=${IMAGE:-k8s-app-standby-toggle:e2e}
here=$(cd "$(dirname "$0")" && pwd)
chart=$here/../../charts/k8s-app-standby-toggle
ann=k8s-app-standby-toggle.cloud64.net
port=18080

step() { printf '\n==> %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; diagnostics; exit 1; }
diagnostics() {
  kubectl -n standby get pods -o wide || true
  kubectl -n standby logs deploy/standby-k8s-app-standby-toggle --tail=80 || true
  kubectl -n demo get deploy,sts,cronjob -o wide || true
}
# wait_for <seconds> <description> <command...>
wait_for() {
  local timeout=$1 what=$2
  shift 2
  for _ in $(seq "$timeout"); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  fail "timed out after ${timeout}s waiting for: $what"
}
replicas() { kubectl -n demo get "$1" -o jsonpath='{.spec.replicas}'; }
annotation() { kubectl -n demo get "$1" -o jsonpath="{.metadata.annotations.${ann//./\\.}/$2}"; }
is() { [[ "$($1 "${@:2:$#-2}")" == "${!#}" ]]; }

body=$(mktemp)
api() { # api <token> <method> <path> [json] → prints the status; the body is in $body
  local token=$1 method=$2 path=$3 json=${4:-}
  local args=(-s -o "$body" -w '%{http_code}' -X "$method" "http://127.0.0.1:$port/api/v1$path")
  if [[ -n $token ]]; then args+=(-H "Authorization: Bearer $token"); fi
  if [[ -n $json ]]; then args+=(-H 'Content-Type: application/json' -d "$json"); fi
  curl "${args[@]}"
}
expect() { # expect <status> <api args...>
  local want=$1
  shift
  local code
  code=$(api "$@")
  [[ $code == "$want" ]] || fail "$2 $3: got $code $(cat "$body"), want $want"
  echo "ok $want $2 $3"
}
group() { api "$CI" GET "/groups/${1:-demo}" >/dev/null && cat "$body"; }
phase_is() { [[ "$(group | jq -r "$1")" == "$2" ]]; }

step "Prerequisites"
kubectl create -f "$ARGOCD_CRD" >/dev/null
kubectl create namespace argocd
kubectl create namespace demo
kubectl create namespace ci
kubectl create namespace standby
# The service must run under the restricted Pod Security Standard (stricter
# than the cluster's baseline).
kubectl label namespace standby pod-security.kubernetes.io/enforce=restricted
kubectl -n ci create serviceaccount ci
kubectl -n ci create serviceaccount intruder
kubectl apply -f "$here/workloads.yaml"
kubectl wait --for=condition=Established crd/applications.argoproj.io --timeout=60s
kubectl apply -f "$here/applications.yaml"
kubectl -n demo rollout status deploy/web --timeout=120s

step "Install the chart"
helm install standby "$chart" -n standby -f "$here/values.yaml" \
  --set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE##*:}" --set image.pullPolicy=Never \
  --wait --timeout 3m
kubectl -n standby port-forward svc/standby-k8s-app-standby-toggle "$port:8080" >/tmp/e2e-port-forward.log 2>&1 &
trap 'kill %1 2>/dev/null || true' EXIT
wait_for 30 "port-forward" curl -sf "http://127.0.0.1:$port/healthz"
CI=$(kubectl -n ci create token ci --audience k8s-app-standby-toggle --duration 1h)
INTRUDER=$(kubectl -n ci create token intruder --audience k8s-app-standby-toggle --duration 1h)
WRONG_AUDIENCE=$(kubectl -n ci create token ci --duration 1h)

step "A group with default asleep is asleep after startup, without any call"
wait_for 60 "web scaled to 0" is replicas deploy/web 0
[[ $(annotation deploy/web replicas) == 2 ]] || fail "web: recorded $(annotation deploy/web replicas), want 2"
[[ $(annotation deploy/worker replicas) == 0 ]] || fail "worker: recorded value 0 missing"
wait_for 30 "cache scaled to 0" is replicas sts/cache 0
wait_for 30 "cleanup suspended" bash -c '[[ $(kubectl -n demo get cronjob/cleanup -o jsonpath={.spec.suspend}) == true ]]'
[[ $(kubectl -n kube-system get deploy/coredns -o jsonpath='{.spec.replicas}') -gt 0 ]] || fail "coredns in kube-system was touched"
wait_for 60 "phase asleep" phase_is .phase asleep

step "Authentication"
expect 401 "" GET /groups
expect 401 "$WRONG_AUDIENCE" GET /groups
expect 403 "$INTRUDER" GET /groups
expect 200 "$CI" GET /groups
expect 404 "$CI" GET /groups/nope

step "Machine permissions and always-on"
expect 403 "$CI" PUT /groups/demo/override '{"state":"awake","ttl":"1h"}'
expect 409 "$CI" PUT /groups/platform/leases/e2e '{"ttl":"5m"}'
[[ $(group platform | jq -r '.missing[0]') == platform ]] || fail "missing Application not reported"

step "A lease wakes the group, and every resource comes back as it was"
expect 200 "$CI" PUT /groups/demo/leases/ci-run-1 '{"ttl":"10m"}'
wait_for 180 "phase awake and ready" phase_is '.phase + " " + (.ready|tostring)' "awake true"
[[ $(replicas deploy/web) == 2 && $(replicas deploy/worker) == 0 && $(replicas sts/cache) == 1 ]] || fail "replicas not restored"
[[ -z $(annotation deploy/web replicas) ]] || fail "annotation kept after wake"
[[ $(kubectl -n demo get cronjob/cleanup -o jsonpath='{.spec.suspend}') == false ]] || fail "cleanup still suspended"

step "Deleting the lease puts it back to sleep"
expect 200 "$CI" DELETE /groups/demo/leases/ci-run-1
wait_for 60 "phase asleep" phase_is .phase asleep
[[ $(replicas deploy/web) == 0 ]] || fail "web awake"

step "A hold keeps the group awake after the lease is gone, then expires"
expect 200 "$CI" PUT /groups/demo/leases/ci-run-2 '{"ttl":"10m"}'
wait_for 180 "phase awake" phase_is .phase awake
expect 200 "$CI" PUT /groups/demo/hold '{"reason":"e2e failed, run 2","ttl":"45s"}'
expect 200 "$CI" DELETE /groups/demo/leases/ci-run-2
expect 403 "$CI" DELETE /groups/demo/hold
[[ $(group | jq -r .reason) == *"e2e failed, run 2"* ]] || fail "hold reason not shown: $(group | jq -r .reason)"
sleep 10
[[ $(replicas deploy/web) == 2 ]] || fail "hold did not keep the group awake"
wait_for 90 "hold expired, phase asleep" phase_is .phase asleep

step "A lease that is never deleted expires"
expect 200 "$CI" PUT /groups/demo/leases/ci-run-3 '{"ttl":"40s"}'
wait_for 120 "phase awake" phase_is .phase awake
wait_for 90 "lease expired, phase asleep" phase_is .phase asleep

step "A Deployment added to a sleeping Application is put to sleep"
kubectl -n demo create deployment late --image=registry.k8s.io/pause:3.10.1 --replicas=1
kubectl -n argocd patch application demo --type=json \
  -p '[{"op":"add","path":"/status/resources/-","value":{"group":"apps","version":"v1","kind":"Deployment","namespace":"demo","name":"late"}}]'
wait_for 30 "late scaled to 0" is replicas deploy/late 0

step "Without the Argo CD prerequisite the group ends in conflict, and the service stops acting"
( while true; do kubectl -n demo scale deploy/web --replicas=2 >/dev/null 2>&1; sleep 2; done ) &
fighter=$!
wait_for 120 "phase conflict" phase_is .phase conflict
kill $fighter
sleep 15
[[ $(replicas deploy/web) == 2 ]] || fail "the service kept acting on a group in conflict"
curl -s "http://127.0.0.1:$port/metrics" | grep -q 'k8s_app_standby_toggle_group_conflict{group="demo"} 1' || fail "conflict metric"
curl -s "http://127.0.0.1:$port/metrics" | grep -q 'k8s_app_standby_toggle_group_should_be_awake{group="platform"} 1' || fail "should_be_awake metric"

step "The state lives in a ConfigMap the service created"
kubectl -n standby get configmap standby-k8s-app-standby-toggle-state -o jsonpath='{.data.state\.json}' | jq -e '.groups.demo.conflict.reason' >/dev/null || fail "state ConfigMap"

step "The pod runs as non-root with a read-only root filesystem"
pod=$(kubectl -n standby get pod -l app.kubernetes.io/name=k8s-app-standby-toggle -o jsonpath='{.items[0].metadata.name}')
kubectl -n standby get pod "$pod" -o json | jq -e '.spec.containers[0].securityContext | .readOnlyRootFilesystem == true and .runAsNonRoot == true and .allowPrivilegeEscalation == false' >/dev/null || fail "securityContext"

printf '\nAll end-to-end checks passed.\n'
