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

# The Job has an activeDeadline too; this keeps a hung CLI from eating all of it silently.
exec timeout 1200 gemini "${args[@]}" -p "$task"
