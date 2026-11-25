#!/bin/bash
# Implementer entrypoint for repository tasks (docs/multi-agent-permissions.md §5.2).
#
# The init container already cloned the repository to /workspace/repo. This script runs the agent in
# it, turns the result into ONE patch (`git diff --binary`) and uploads it to the Control Plane. It never
# commits or pushes: this pod has no credentials for GitHub. Success is this process's exit code.
set -uo pipefail

REPO_DIR=/workspace/repo
OUT_DIR=/workspace/out
MAX_PATCH="${AAP_MAX_PATCH_BYTES:-1048576}"
export HOME=/tmp/home
mkdir -p "$HOME" "$OUT_DIR"

post_event() { # message (fixed text: no quotes or newlines)
  [ -n "${AAP_CONTROL_PLANE_URL:-}" ] || return 0
  curl -fsS -m 5 -X POST "$AAP_CONTROL_PLANE_URL/internal/v1/tasks/$AAP_TASK_ID/events" \
    -H "Authorization: Bearer ${AAP_PROGRESS_TOKEN:-}" -H 'Content-Type: application/json' \
    -d "$(printf '{"phase":"PROGRESS","message":"%s"}' "$1")" >/dev/null 2>&1 || true
}
die() { echo "ERROR: $*" >&2; post_event "$1"; exit 1; }

cd "$REPO_DIR" || die "リポジトリがありません"

# Global agent instructions live in $HOME, not in the repository: they must not end up in the patch.
mkdir -p "$HOME/.gemini"
cp /opt/aap/AGENTS.task.md "$HOME/.gemini/GEMINI.md"
# security.auth.selectedType is needed: with GOOGLE_GEMINI_BASE_URL set (LLM gateway) the CLI would auto-select its "gateway"
# auth type, which its own headless validation rejects ("Invalid auth method selected."). With gemini-api-key it still sends
# GEMINI_API_KEY (here: the task token) to GOOGLE_GEMINI_BASE_URL. Checked against the CLI by hack/check-gemini-gateway.sh.
cat > "$HOME/.gemini/settings.json" <<'JSON'
{
  "security": { "auth": { "selectedType": "gemini-api-key" } },
  "general": { "disableAutoUpdate": true, "disableUpdateNag": true },
  "privacy": { "usageStatisticsEnabled": false },
  "telemetry": { "enabled": false },
  "tools": { "exclude": ["google_web_search", "web_fetch"] }
}
JSON

post_event "作業をしています"
echo "== agent=${AAP_AGENT} task=${AAP_TASK_ID} repo=${AAP_REPO}"
"/opt/aap/agents/task-${AAP_AGENT}.sh"; rc=$?
[ "$rc" -eq 0 ] || die "作業の途中で止まりました"

# Build the patch. Hooks and external diff drivers are switched off: the repository content is the agent's.
post_event "差分を作っています"
G=(git -c core.hooksPath=/dev/null -c core.fsmonitor=false -c core.pager=cat)
"${G[@]}" add -A -- . ':(exclude)node_modules' || die "差分を作れませんでした"
"${G[@]}" diff --cached --binary --no-color --no-ext-diff --no-textconv > "$OUT_DIR/patch.diff" || die "差分を作れませんでした"

size=$(wc -c < "$OUT_DIR/patch.diff")
if [ "$size" -eq 0 ]; then
  echo "== no changes"
  post_event "変更はありませんでした"
  exit 0
fi
[ "$size" -le "$MAX_PATCH" ] || die "差分が大きすぎます"

post_event "差分を送っています"
curl -fsS -m 60 -X POST "$AAP_CONTROL_PLANE_URL/internal/v1/tasks/$AAP_TASK_ID/patch" \
  -H "Authorization: Bearer ${AAP_PROGRESS_TOKEN:-}" -H 'Content-Type: application/octet-stream' \
  --data-binary @"$OUT_DIR/patch.diff" > "$OUT_DIR/upload.json" || die "差分を送れませんでした"
echo "== uploaded $size bytes: $(cat "$OUT_DIR/upload.json")"
exit 0
