#!/usr/bin/env bash
# End-to-end check of Stage 1 on a throw-away kind cluster.
#   hack/e2e-kind.sh            # create cluster aap-dev if missing, deploy, run scenarios
#   KEEP=1 hack/e2e-kind.sh     # leave the platform running afterwards
# Never touches any kube context other than kind-aap-dev.
set -euo pipefail
cd "$(dirname "$0")/.."
CTX=kind-aap-dev
KP=${KP_DIR:-$HOME/dev/kubernetes-platform}
K="kubectl --context $CTX"
API=http://127.0.0.1:18080
say() { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
ok()  { printf '\033[32m  ok\033[0m %s\n' "$*"; }
fail(){ printf '\033[31m  FAIL\033[0m %s\n' "$*"; exit 1; }

# Fresh cluster every run (state in SQLite/PVCs would otherwise leak between runs).
# REUSE=1 keeps an existing aap-dev. Only the cluster named aap-dev is ever deleted.
[ -n "${REUSE:-}" ] || kind delete cluster --name aap-dev >/dev/null 2>&1 || true
kind get clusters | grep -qx aap-dev || kind create cluster --name aap-dev --wait 120s
# Pre-pull the helper image once (see dev-log: first pull can stall for minutes).
docker pull -q busybox:1.37 >/dev/null 2>&1 && kind load docker-image --name aap-dev busybox:1.37 >/dev/null 2>&1 || true

say "build + load images"
docker build -q -t aap-control-plane:dev control-plane >/dev/null
docker build -q -t aap-agent-runtime:dev agent-runtime >/dev/null
docker build -q -t aap-app-runtime:dev app-runtime >/dev/null
docker build -q -t aap-portal:dev portal >/dev/null
kind load docker-image --name aap-dev aap-control-plane:dev aap-agent-runtime:dev aap-app-runtime:dev aap-portal:dev >/dev/null

say "deploy platform manifests from kubernetes-platform"
kubectl kustomize "$KP/manifests/ai-app-platform" \
 | sed -e 's#local-path#standard#g' \
       -e 's#ghcr.io/k-kanke/ai-app-platform/control-plane:latest#aap-control-plane:dev#' \
       -e 's#ghcr.io/k-kanke/ai-app-platform/agent-runtime:latest#aap-agent-runtime:dev#' \
       -e 's#ghcr.io/k-kanke/ai-app-platform/app-runtime:latest#aap-app-runtime:dev#' \
       -e 's#ghcr.io/k-kanke/ai-app-platform/portal:latest#aap-portal:dev#' \
 | $K apply -f - >/dev/null
$K -n platform-system create secret generic control-plane-secrets --from-literal=token-secret=e2e-secret --dry-run=client -o yaml | $K apply -f - >/dev/null
$K -n platform-system set env deploy/control-plane AAP_RUNTIME_TIMEOUT_SECONDS=75 AAP_AGENT=template >/dev/null
$K -n platform-system rollout restart deploy/control-plane >/dev/null
$K -n platform-system rollout status deploy/control-plane --timeout=120s

pkill -f "port-forward.*18080" 2>/dev/null || true
$K -n platform-system port-forward svc/control-plane 18080:8080 >/tmp/aap-pf.log 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null || true; kill ${PF2:-} 2>/dev/null || true' EXIT
for i in $(seq 30); do curl -fs $API/healthz >/dev/null 2>&1 && break; sleep 1; done
curl -fs $API/healthz >/dev/null || fail "control plane not reachable"

phase() { curl -fs $API/api/v1/apps/$1 | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d["phase"],(d.get("operation") or {}).get("state",""))'; }
wait_ready() { # app timeout
  for i in $(seq "${2:-180}"); do
    p=$(phase "$1" || true)
    [ "$p" = "READY SUCCEEDED" ] || [ "$p" = "READY FAILED" ] && { echo "$p"; return 0; }
    case "$p" in FAILED*) echo "$p"; return 1;; esac
    sleep 1
  done; echo "timeout ($p)"; return 1
}
post() { curl -fsS -X POST "$API$1" -H 'Content-Type: application/json' -d "$2"; }

say "1. create app from natural language"
post /api/v1/apps '{"id":"meal","name":"ご飯管理","prompt":"家族4人の昼ご飯と夜ご飯の予定表を作って"}' >/dev/null
r=$(wait_ready meal 240) || { $K -n aap-apps get all,pvc; fail "create: $r"; }
[ "$r" = "READY SUCCEEDED" ] && ok "meal is READY" || fail "create result: $r"

say "2. use the app (write via API, read back)"
$K -n aap-apps port-forward svc/aap-gen-meal 18081:80 >/tmp/aap-pf2.log 2>&1 &
PF2=$!; sleep 3
curl -fs localhost:18081/healthz >/dev/null && ok "/healthz"
curl -fsS -X PUT localhost:18081/api/meals -H 'Content-Type: application/json' -d '{"member":"母","date":"2026-10-05","slot":"dinner","value":"yes"}' >/dev/null
curl -fs localhost:18081/api/meals | grep -q '2026-10-05|dinner|母' && ok "answer saved to /data" || fail "answer not saved"

say "3. isolation: Agent saw source only, not data"
agentjob=$($K -n aap-apps get jobs -l aap.dev/role=agent -o jsonpath='{.items[0].metadata.name}')
vols=$($K -n aap-apps get job "$agentjob" -o jsonpath='{.spec.template.spec.volumes[*].persistentVolumeClaim.claimName}')
[ "$vols" = "aap-gen-meal-src" ] && ok "agent mounts only $vols" || fail "agent volumes: $vols"
$K -n aap-apps logs "job/$agentjob" | tail -3

say "4. modify app; existing data must survive"
post /api/v1/apps/meal/changes '{"prompt":"文字を大きくして"}' >/dev/null
r=$(wait_ready meal 180) || fail "modify: $r"
[ "$r" = "READY SUCCEEDED" ] && ok "modify succeeded" || fail "modify result: $r"
kill $PF2 2>/dev/null || true; $K -n aap-apps port-forward svc/aap-gen-meal 18081:80 >/tmp/aap-pf2.log 2>&1 & PF2=$!; sleep 3
curl -fs localhost:18081/api/meals | grep -q '2026-10-05|dinner|母' && ok "data survived the update" || fail "DATA LOST"
$K -n aap-apps get pod -l aap.dev/app-id=meal,aap.dev/role=runtime -o name | head -1

say "5. bad change (agent breaks the app) is rolled back"
post /api/v1/apps/meal/changes '{"prompt":"BREAK_APP"}' >/dev/null
for i in $(seq 240); do
  s=$(curl -fs $API/api/v1/apps/meal/operations | python3 -c 'import sys,json;o=json.load(sys.stdin)["operations"][0];print(o["state"])')
  [ "$s" = FAILED ] || [ "$s" = SUCCEEDED ] && break; sleep 1; done
[ "$s" = FAILED ] && ok "operation FAILED as expected" || fail "operation ended $s"
for i in $(seq 90); do [ "$(phase meal | cut -d' ' -f1)" = READY ] && break; sleep 1; done
kill $PF2 2>/dev/null || true; $K -n aap-apps port-forward svc/aap-gen-meal 18081:80 >/tmp/aap-pf2.log 2>&1 & PF2=$!; sleep 3
curl -fs localhost:18081/healthz >/dev/null && ok "previous version is serving again" || fail "app not serving after rollback"
curl -fs localhost:18081/api/meals | grep -q '2026-10-05|dinner|母' && ok "data intact" || fail "DATA LOST"

say "5b. change that passes tests but does not start is rolled back (no test script in app)"
post /api/v1/apps '{"id":"note","name":"メモ帳","prompt":"家族で使えるメモ帳"}' >/dev/null
r=$(wait_ready note 240) || fail "note create: $r"; ok "note is READY"
post /api/v1/apps/note/changes '{"prompt":"BREAK_APP"}' >/dev/null
for i in $(seq 240); do
  s=$(curl -fs $API/api/v1/apps/note/operations | python3 -c 'import sys,json;print(json.load(sys.stdin)["operations"][0]["state"])')
  [ "$s" = FAILED ] || [ "$s" = SUCCEEDED ] && break; sleep 1; done
[ "$s" = FAILED ] && ok "operation FAILED (runtime never became ready)" || fail "note op ended $s"
for i in $(seq 120); do [ "$(phase note | cut -d' ' -f1)" = READY ] && break; sleep 1; done
$K -n aap-apps rollout status deploy/aap-gen-note --timeout=120s >/dev/null && ok "restored version is serving again" || fail "note not restored"
$K -n aap-apps get jobs -l aap.dev/app-id=note,aap.dev/role=restore -o name | grep -q restore && ok "restore job ran"

say "6. Control Plane restart mid-operation (Experiment B)"
post /api/v1/apps '{"id":"memo","name":"メモ","prompt":"家族で使えるメモ帳"}' >/dev/null
sleep 2
old=$($K -n platform-system get pod -l app.kubernetes.io/name=control-plane -o jsonpath='{.items[0].metadata.name}')
$K -n platform-system delete pod "$old" --wait=false >/dev/null
# Wait for the OLD pod to be gone and the NEW one Ready; `rollout status` alone can return
# while the old pod is still terminating (the port-forward would then bind to it and die).
$K -n platform-system wait --for=delete pod/"$old" --timeout=120s >/dev/null
$K -n platform-system wait --for=condition=Ready pod -l app.kubernetes.io/name=control-plane --timeout=120s >/dev/null
kill $PF 2>/dev/null || true; $K -n platform-system port-forward svc/control-plane 18080:8080 >/tmp/aap-pf.log 2>&1 & PF=$!
for i in $(seq 30); do curl -fs $API/healthz >/dev/null 2>&1 && break; sleep 1; done
r=$(wait_ready memo 240) || fail "memo after restart: $r"
n=$($K -n aap-apps get jobs -l aap.dev/app-id=memo,aap.dev/role=agent -o name | wc -l | tr -d ' ')
[ "$r" = "READY SUCCEEDED" ] && [ "$n" = 1 ] && ok "recovered after restart ($n agent job)" || fail "restart recovery: $r jobs=$n"

say "7. delete keeps data unless purged"
curl -fsS -X DELETE $API/api/v1/apps/memo >/dev/null; sleep 6
$K -n aap-apps get pvc aap-gen-memo-data >/dev/null && ok "data PVC kept after delete" || fail "PVC deleted"
$K -n aap-apps get deploy aap-gen-memo >/dev/null 2>&1 && fail "runtime still exists" || ok "runtime removed"

say "8. failures explain themselves in plain language"
post /api/v1/apps '{"id":"quota","name":"上限","prompt":"FAIL_QUOTA 天気アプリ"}' >/dev/null
for i in $(seq 120); do [ "$(phase quota | cut -d' ' -f1)" = FAILED ] && break; sleep 2; done
msg=$(curl -fs $API/api/v1/apps/quota | python3 -c 'import sys,json;print(json.load(sys.stdin)["operation"].get("userMessage",""))')
echo "  user message: $msg"
case "$msg" in *利用上限*) ok "spending-cap failure is explained";; *) fail "message was: $msg";; esac
curl -fs $API/api/v1/apps/quota | python3 -c 'import sys,json;d=json.load(sys.stdin);sys.exit(1 if "detail" in (d.get("operation") or {}) else 0)' && ok "raw logs are not exposed to users" || fail "operator detail leaked into the app view"
curl -fs $API/api/v1/apps/quota/operations | grep -q 'spending cap' && ok "raw log kept for operators" || fail "operator detail missing"

say "9. retry a failed create from scratch with a reworded request"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST $API/api/v1/apps/quota/retry -H 'Content-Type: application/json' -d '{"prompt":"家族の天気アプリ"}')
[ "$code" = 202 ] && ok "retry accepted" || fail "retry status $code"
r=$(wait_ready quota 240) || fail "retry: $r"
[ "$r" = "READY SUCCEEDED" ] && ok "retried app is READY" || fail "retry result: $r"
$K -n aap-apps get jobs -l aap.dev/app-id=quota,aap.dev/role=wipe -o name | grep -q wipe && ok "source was wiped before the retry" || fail "no wipe job"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST $API/api/v1/apps/quota/retry); [ "$code" = 409 ] && ok "a READY app cannot be 'retried'" || fail "expected 409, got $code"

say "10. delete with purge removes the data too"
curl -fsS -X DELETE "$API/api/v1/apps/quota?purge=true" >/dev/null
for i in $(seq 30); do [ "$(curl -s -o /dev/null -w '%{http_code}' $API/api/v1/apps/quota)" = 404 ] && break; sleep 2; done
sleep 8
$K -n aap-apps get pvc aap-gen-quota-data >/dev/null 2>&1 && fail "data PVC still exists" || ok "data PVC removed"

say "ALL SCENARIOS PASSED"
if [ -z "${KEEP:-}" ]; then
  echo "(cluster aap-dev left running; remove with: kind delete cluster --name aap-dev)"
fi
