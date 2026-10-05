#!/bin/bash
# Agent Job entrypoint. Runs one create/modify request against /workspace.
# Success/failure is decided by this process's exit code (Job status), the
# progress events below are advisory for the UI only.
set -uo pipefail

# ---- latency trace (docs/deep-dive-inner-loop-latency.md §5) -----------------------------
# Epoch milliseconds. One JSON line, "AAP_TRACE {...}", is printed on exit (success or failure);
# the Control Plane reads it from the pod log. Missing points are null.
now_ms() { local t=${EPOCHREALTIME/./}; echo $((t / 1000)); }
T_ENTRY=$(now_ms); T_AGENT_START=""; T_AGENT_END=""; T_CHECKS_END=""; T_TEST_START=""; T_TEST_END=""
WATCH_DIR=/tmp/aap-watch; WATCH_PID=""

# Watch /workspace for the AGENT's writes (first / last / count). The platform's own files are
# excluded, and node_modules/.git are not watched at all.
start_watch() {
  rm -rf "$WATCH_DIR"; mkdir -p "$WATCH_DIR"
  command -v inotifywait >/dev/null 2>&1 || return 0
  (
    n=0
    inotifywait -m -r -q -e close_write,create,moved_to,delete \
      --exclude '(^|/)(node_modules|\.git)(/|$)' --format '%w%f' /workspace 2>/dev/null |
    while read -r path; do
      case "$path" in /workspace/AGENTS.md|/workspace/GEMINI.md) continue;; esac
      t=$(now_ms); [ -f "$WATCH_DIR/first" ] || echo "$t" > "$WATCH_DIR/first"
      echo "$t" > "$WATCH_DIR/last"; n=$((n + 1)); echo "$n" > "$WATCH_DIR/n"
    done
  ) &
  WATCH_PID=$!
}
stop_watch() {
  [ -n "$WATCH_PID" ] || return 0
  sleep 0.2   # let in-flight events drain
  pkill -P "$WATCH_PID" 2>/dev/null; kill "$WATCH_PID" 2>/dev/null; WATCH_PID=""
}
j() { [ -n "${1:-}" ] && printf '%s' "$1" || printf 'null'; }
emit_trace() {
  local rc=$?
  stop_watch
  local first="" last="" n=0
  [ -f "$WATCH_DIR/first" ] && first=$(cat "$WATCH_DIR/first")
  [ -f "$WATCH_DIR/last" ] && last=$(cat "$WATCH_DIR/last")
  [ -f "$WATCH_DIR/n" ] && n=$(cat "$WATCH_DIR/n")
  printf 'AAP_TRACE {"v":1,"app":"%s","op":"%s","kind":"%s","agent":"%s","model":"%s","rc":%s,' \
    "${AAP_APP_ID:-}" "${AAP_OP_ID:-}" "${AAP_OP_KIND:-}" "${AAP_AGENT:-}" "${AAP_GEMINI_MODEL:-}" "$rc"
  printf '"t_entry":%s,"t_agent_start":%s,"t_first_write":%s,"t_last_write":%s,"writes":%s,' \
    "$(j "$T_ENTRY")" "$(j "$T_AGENT_START")" "$(j "$first")" "$(j "$last")" "$n"
  printf '"t_agent_end":%s,"t_checks_end":%s,"t_test_start":%s,"t_test_end":%s,"t_exit":%s}\n' \
    "$(j "$T_AGENT_END")" "$(j "$T_CHECKS_END")" "$(j "$T_TEST_START")" "$(j "$T_TEST_END")" "$(now_ms)"
}
trap emit_trace EXIT

post_event() { # phase message
  [ -n "${AAP_CONTROL_PLANE_URL:-}" ] || return 0
  curl -fsS -m 5 -X POST "$AAP_CONTROL_PLANE_URL/internal/v1/operations/$AAP_OP_ID/events" \
    -H "Authorization: Bearer ${AAP_PROGRESS_TOKEN:-}" -H 'Content-Type: application/json' \
    -d "$(printf '{"phase":"%s","message":"%s"}' "$1" "$2")" >/dev/null 2>&1 || true
}
die() { echo "ERROR: $*" >&2; post_event "$1" "$2"; exit 1; }

cd /workspace || exit 1
mkdir -p "$HOME"
# Same instructions under every name an agent CLI looks for (platform-owned content).
cp /opt/aap/AGENTS.md /workspace/AGENTS.md
cp /opt/aap/AGENTS.md /workspace/GEMINI.md

# Gemini CLI: no telemetry / usage statistics / update checks from inside the Job.
mkdir -p "$HOME/.gemini"
cat > "$HOME/.gemini/settings.json" <<'JSON'
{
  "general": { "disableAutoUpdate": true, "disableUpdateNag": true },
  "privacy": { "usageStatisticsEnabled": false },
  "telemetry": { "enabled": false }
}
JSON

post_event GENERATING "アプリを作っています"
echo "== agent=${AAP_AGENT} kind=${AAP_OP_KIND} app=${AAP_APP_ID}"
start_watch
T_AGENT_START=$(now_ms)
"/opt/aap/agents/${AAP_AGENT}.sh"; agent_rc=$?
T_AGENT_END=$(now_ms); stop_watch
[ "$agent_rc" -eq 0 ] || die GENERATING "アプリを作る途中で止まりました"

# --- App Contract checks (platform-owned, the Agent cannot skip them) ---
[ -f package.json ] || die GENERATING "package.json がありません"
node -e 'const p=require("./package.json"); process.exit(p.scripts&&p.scripts.start?0:1)' \
  || die GENERATING "npm start が定義されていません"

if node -e 'const p=require("./package.json"); process.exit(Object.keys(p.dependencies||{}).length?0:1)'; then
  post_event GENERATING "部品を準備しています"
  npm install --omit=dev --no-audit --no-fund || die GENERATING "部品の準備に失敗しました"
fi
T_CHECKS_END=$(now_ms)

if node -e 'const p=require("./package.json"); process.exit(p.scripts&&p.scripts.test?0:1)'; then
  post_event TESTING "動作を確認しています"
  T_TEST_START=$(now_ms)
  npm test || { T_TEST_END=$(now_ms); die TESTING "確認テストに失敗しました"; }
  T_TEST_END=$(now_ms)
fi
post_event TESTING "確認できました"
echo "== done"
