#!/bin/bash
# Real agent: Gemini CLI (https://github.com/google-gemini/gemini-cli), headless mode.
# Needs an image built with --build-arg INSTALL_GEMINI=true and GEMINI_API_KEY
# provided through the Agent Secret (agent-credentials).
# Optional: AAP_GEMINI_MODEL to pick a model; otherwise the CLI default is used.
set -euo pipefail
cd /workspace
: "${GEMINI_API_KEY:?GEMINI_API_KEY is not set (Agent Secret agent-credentials)}"

if [ "${AAP_OP_KIND}" = "create" ]; then
  task="次の依頼のアプリを新しく作ってください。このディレクトリ(/workspace)の中だけで作業し、GEMINI.md の App Contract を必ず守ってください。作り終えたら npm test(あれば)を実行して確認してください。

依頼: ${AAP_PROMPT}"
else
  task="既存のアプリを、次の依頼どおりに変更してください。このディレクトリ(/workspace)の中だけで作業してください。既存のデータ形式を壊さず、依頼されていない部分は変えないでください。GEMINI.md の App Contract を守り、変更後に npm test(あれば)を実行して確認してください。

依頼: ${AAP_PROMPT}"
fi

args=(--approval-mode yolo --output-format text)
[ -n "${AAP_GEMINI_MODEL:-}" ] && args+=(--model "$AAP_GEMINI_MODEL")

# Gemini CLI retries a failing request ~10 times with backoff (minutes). Errors that retrying
# cannot fix (spending cap, bad key) must fail fast so the family sees the reason right away.
log=$(mktemp)
timeout 1200 gemini "${args[@]}" -p "$task" > >(tee -a "$log") 2>&1 &
pid=$!
( while kill -0 "$pid" 2>/dev/null; do
    if grep -qiE "spending cap|API key not valid|API_KEY_INVALID|PERMISSION_DENIED|exceeded your current quota" "$log"; then
      echo "AAP: unrecoverable API error detected, stopping the agent" >&2
      kill "$pid" 2>/dev/null; exit 0
    fi
    sleep 2
  done ) &
watcher=$!
wait "$pid"; rc=$?
kill "$watcher" 2>/dev/null || true
exit "$rc"
