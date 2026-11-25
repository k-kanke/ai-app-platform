#!/usr/bin/env bash
# End-to-end check of repository tasks (Phase 1-2 of docs/multi-agent-execution-plan.md) on kind.
#   hack/e2e-tasks-kind.sh            # reuse cluster aap-dev (created if missing), deploy, run scenarios
# Uses the offline stub agent (AAP_AGENT=template): no API key, no LLM. The clone is a real one from GitHub
# (a public repository), so the kind nodes need internet access. Never touches any context other than kind-aap-dev.
set -euo pipefail
cd "$(dirname "$0")/.."
CTX=kind-aap-dev
KP=${KP_DIR:-$HOME/dev/kubernetes-platform}
REPO=${E2E_REPO:-k-kanke/genkan}
K="kubectl --context $CTX"
API=http://127.0.0.1:18082
say() { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
ok()  { printf '\033[32m  ok\033[0m %s\n' "$*"; }
fail(){ printf '\033[31m  FAIL\033[0m %s\n' "$*"; $K -n aap-agents get all,pvc 2>/dev/null | head -20; exit 1; }

kind get clusters | grep -qx aap-dev || kind create cluster --name aap-dev --wait 120s

say "build + load images"
if docker image inspect golang:1.26-alpine >/dev/null 2>&1; then
  docker build -q -t aap-control-plane:dev control-plane >/dev/null
else
  # The Go base image is not local and a first pull can stall for minutes (docs/dev-log.md): build the binary on
  # the host for the kind node's architecture and put it on a base image that is already here.
  echo "  golang base image not local: cross-compiling on the host"
  arch=$(uname -m); [ "$arch" = x86_64 ] && arch=amd64 || arch=arm64
  b=$(mktemp -d)
  (cd control-plane && GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$b/control-plane" ./cmd/control-plane)
  printf 'FROM alpine:3.20\nCOPY control-plane /control-plane\nUSER 65532:65532\nENTRYPOINT ["/control-plane"]\n' > "$b/Dockerfile"
  docker build -q -t aap-control-plane:dev "$b" >/dev/null
fi
docker build -q -t aap-agent-runtime:dev agent-runtime >/dev/null
kind load docker-image --name aap-dev aap-control-plane:dev aap-agent-runtime:dev >/dev/null

say "deploy platform manifests from kubernetes-platform (includes aap-agents)"
kubectl kustomize "$KP/manifests/ai-app-platform" \
 | sed -e 's#local-path#standard#g' \
       -e 's#ghcr.io/k-kanke/ai-app-platform/control-plane:latest#aap-control-plane:dev#' \
       -e 's#ghcr.io/k-kanke/ai-app-platform/agent-runtime:latest#aap-agent-runtime:dev#' \
       -e 's#ghcr.io/k-kanke/ai-app-platform/app-runtime:latest#aap-app-runtime:dev#' \
       -e 's#ghcr.io/k-kanke/ai-app-platform/portal:latest#aap-portal:dev#' \
 | $K apply -f - >/dev/null
$K -n platform-system create secret generic control-plane-secrets --from-literal=token-secret=e2e-secret --dry-run=client -o yaml | $K apply -f - >/dev/null
$K -n platform-system set env deploy/control-plane AAP_AGENT=template AAP_TASKS_ENABLED=true AAP_TASK_RETENTION_HOURS=24 >/dev/null
$K -n platform-system rollout restart deploy/control-plane >/dev/null
$K -n platform-system rollout status deploy/control-plane --timeout=120s

# a dev cluster accumulates tasks between runs; start from an empty namespace so the quota is not the subject of this test
$K -n aap-agents delete job,pvc --all --wait=false >/dev/null 2>&1 || true
pkill -f "port-forward.*18082" 2>/dev/null || true
$K -n platform-system port-forward svc/control-plane 18082:8080 >/tmp/aap-tasks-pf.log 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null || true' EXIT
for i in $(seq 30); do curl -fs $API/healthz >/dev/null 2>&1 && break; sleep 1; done
curl -fs $API/healthz >/dev/null || fail "control plane not reachable"

jq_() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)"; }
new_task() { curl -fsS -X POST $API/api/v1/tasks -H 'Content-Type: application/json' -d "{\"repo\":\"$REPO\",\"prompt\":\"$1\"}" | jq_ 'd["id"]'; }
state() { curl -fs $API/api/v1/tasks/$1 | jq_ 'd["state"]'; }
wait_state() { # id want timeout
  local s=""
  for i in $(seq "${3:-240}"); do
    s=$(state "$1" || true)
    [ "$s" = "$2" ] && { echo "$s"; return 0; }
    case "$s" in FAILED|NO_CHANGES|APPROVED|REJECTED|EXPIRED) echo "$s"; return 1;; esac
    sleep 1
  done; echo "timeout ($s)"; return 1
}

say "1. a task clones the repository, changes it, and waits for approval"
T1=$(new_task "READMEに1行足して")
r=$(wait_state "$T1" AWAITING_APPROVAL 240) || { curl -s $API/api/v1/tasks/$T1; fail "task 1: $r"; }
ok "task $T1 is AWAITING_APPROVAL"
info=$(curl -fs $API/api/v1/tasks/$T1)
echo "$info" | jq_ '"files=" + ",".join(f["path"] for f in d["summary"]["files"]) + " added=" + str(d["summary"]["added"])'
[ "$(echo "$info" | jq_ 'd["summary"]["files"][0]["path"]')" = "README.md" ] && ok "the patch touches README.md" || fail "unexpected patch files"
SHA=$(echo "$info" | jq_ 'd["patchSha256"]')
curl -fs $API/api/v1/tasks/$T1/patch | grep -q "stub agent: READMEに1行足して" && ok "the stored patch carries the agent's change" || fail "patch content"
[ "$(curl -fsI $API/api/v1/tasks/$T1/patch | tr -d '\r' | awk -F': ' 'tolower($1)=="x-patch-sha256"{print $2}')" = "$SHA" ] && ok "patch hash matches the task record" || fail "patch hash header"

say "2. isolation of the Implementer pod"
job=aap-task-$T1-impl
sa=$($K -n aap-agents get job $job -o jsonpath='{.spec.template.spec.automountServiceAccountToken}')
[ "$sa" = "false" ] && ok "no ServiceAccount token" || fail "automountServiceAccountToken=$sa"
vols=$($K -n aap-agents get job $job -o jsonpath='{.spec.template.spec.volumes[*].name}')
[ "$vols" = "workspace tmp" ] && ok "volumes: $vols (no Secret, no hostPath)" || fail "volumes: $vols"
ro=$($K -n aap-agents get job $job -o jsonpath='{.spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem}')
[ "$ro" = "true" ] && ok "read-only root filesystem" || fail "readOnlyRootFilesystem=$ro"
pvcs=$($K -n aap-agents get pvc -o name | grep -c "aap-task-$T1-ws" || true)
[ "$pvcs" = "1" ] && ok "workspace PVC exists" || fail "workspace PVC missing"

say "3. approval is tied to the patch hash"
c=$(curl -s -o /dev/null -w '%{http_code}' -X POST $API/api/v1/tasks/$T1/approve -H 'Content-Type: application/json' -d '{"patchSha256":"deadbeef"}')
[ "$c" = "409" ] && ok "a wrong hash is refused (409)" || fail "wrong hash gave $c"
c=$(curl -s -o /dev/null -w '%{http_code}' -X POST $API/api/v1/tasks/$T1/approve -H 'Content-Type: application/json' -d "{\"patchSha256\":\"$SHA\"}")
[ "$c" = "200" ] && [ "$(state $T1)" = "APPROVED" ] && ok "the right hash approves" || fail "approve gave $c"

say "4. no change / failure"
T2=$(new_task "NO_CHANGE")
r=$(wait_state "$T2" NO_CHANGES 240) || fail "no-change task: $r"
ok "a task that changes nothing ends as NO_CHANGES"
T3=$(new_task "FAIL_TASK")
r=$(wait_state "$T3" FAILED 240) || [ "$r" = "FAILED" ] || fail "failing task: $r"
msg=$(curl -fs $API/api/v1/tasks/$T3 | jq_ 'd.get("userMessage","")')
[ -n "$msg" ] && ok "failure is explained: $msg" || fail "failure has no message"

say "5. the workspace is released on success and kept on failure"
# Deleting a PVC is asynchronous (it stays Terminating until the pvc-protection finalizer goes): gone or Terminating = released.
released() { local o; o=$($K -n aap-agents get pvc "aap-task-$1-ws" -o jsonpath='{.metadata.deletionTimestamp}' 2>&1) || return 0; [ -n "$o" ]; }
for i in $(seq 20); do released "$T1" && released "$T2" && break; sleep 1; done
released "$T2" && ok "a task that changed nothing released its workspace (the patch is in the Control Plane)" || fail "a finished task still holds its workspace"
released "$T1" && ok "a task awaiting approval / approved released it too" || fail "an approved task still holds its workspace"
released "$T3" && fail "the failed task lost its workspace" || ok "a failed task keeps its workspace for debugging (until retention)"

say "6. admission policies hold (server-side dry-run)"
if $K get validatingadmissionpolicy aap-agents-pods >/dev/null 2>&1; then
  pod() { printf '{"apiVersion":"v1","kind":"Pod","metadata":{"name":"t","namespace":"aap-agents","labels":{"aap.dev/role":"implementer"}},"spec":{"automountServiceAccountToken":false,"restartPolicy":"Never","securityContext":{"seccompProfile":{"type":"RuntimeDefault"}},%s"containers":[{"name":"c","image":"busybox","resources":{"limits":{"cpu":"100m","memory":"64Mi"}},"securityContext":{"runAsNonRoot":true,"runAsUser":1000,"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}' "$1"; }
  pod '' | $K create --dry-run=server -f - >/dev/null 2>&1 && ok "a conforming pod is admitted" || fail "a conforming pod was refused"
  # capture first: with pipefail, `grep -q` ending the pipe early would make the whole pipeline look failed
  out=$(pod '"volumes":[{"name":"s","secret":{"secretName":"x"}}],' | $K create --dry-run=server -f - 2>&1 || true)
  echo "$out" | grep -q "denied request" && ok "a Secret volume is refused" || fail "Secret volume admitted: $out"
  out=$(pod '"hostNetwork":true,' | $K create --dry-run=server -f - 2>&1 || true)
  echo "$out" | grep -qiE "denied|forbidden" && ok "hostNetwork is refused" || fail "hostNetwork admitted: $out"
else
  echo "  (skipped: this kind cluster has no ValidatingAdmissionPolicy)"
fi

say "all task scenarios passed"
