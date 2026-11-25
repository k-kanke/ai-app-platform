#!/bin/bash
# Real agent for repository tasks: Gemini CLI in headless mode (https://github.com/google-gemini/gemini-cli).
# Needs an image built with INSTALL_GEMINI=true and GEMINI_API_KEY (Phase 3 replaces the key with an LLM gateway).
# Optional: AAP_GEMINI_MODEL. Same failure handling as agents/gemini.sh (kept separate so the family-app flow is untouched).
set -euo pipefail
cd /workspace/repo
: "${GEMINI_API_KEY:?GEMINI_API_KEY is not set}"

task="このリポジトリ(${AAP_REPO})の作業ディレクトリで、次の依頼を実装してください。
コミットや push はしないでください。変更後、テストやビルドの手順があれば実行して確認してください。
最後に、何を変えたかを日本語で短く説明してください。

依頼: ${AAP_PROMPT}"

args=(--approval-mode yolo --output-format text)
[ -n "${AAP_GEMINI_MODEL:-}" ] && args+=(--model "$AAP_GEMINI_MODEL")

log=$(mktemp)
# The watcher below kills the CLI when retrying cannot help. The CLI answers SIGTERM by exiting 0, so the kill alone would look
# like success (and a task with no changes would be reported as "no changes"). The watcher leaves a marker; we fail on it.
aborted="$log.aborted"
timeout "${AAP_AGENT_STEP_TIMEOUT_S:-900}" gemini "${args[@]}" -p "$task" > >(tee -a "$log") 2>&1 &
pid=$!
( while kill -0 "$pid" 2>/dev/null; do
    if grep -qiE "spending cap|API key not valid|API_KEY_INVALID|PERMISSION_DENIED|exceeded your current quota" "$log"; then
      echo "AAP: unrecoverable API error detected, stopping the agent" >&2
      echo unrecoverable > "$aborted"; kill "$pid" 2>/dev/null; exit 0
    fi
    if [ "$(grep -ciE "failed with status 5[0-9][0-9]" "$log")" -ge "${AAP_AGENT_MAX_5XX:-6}" ]; then
      echo "AAP: the API keeps failing with 5xx, stopping the agent" >&2
      echo api-5xx > "$aborted"; kill "$pid" 2>/dev/null; exit 0
    fi
    sleep 2
  done ) &
watcher=$!
wait "$pid" && rc=0 || rc=$?
kill "$watcher" 2>/dev/null || true
if [ -e "$aborted" ]; then
  echo "AAP: the agent was stopped ($(cat "$aborted")); this is a failure, not a result" >&2
  exit 70
fi
exit "$rc"
