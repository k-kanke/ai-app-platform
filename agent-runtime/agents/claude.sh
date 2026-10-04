#!/bin/bash
# Real agent: Claude Code CLI. Requires an image built with
# --build-arg INSTALL_CLAUDE=true and ANTHROPIC_API_KEY provided via the Agent Secret.
set -euo pipefail
cd /workspace
: "${ANTHROPIC_API_KEY:?ANTHROPIC_API_KEY is not set (Agent Secret)}"
if [ "${AAP_OP_KIND}" = "create" ]; then
  task="次の依頼のアプリを新しく作ってください。AGENTS.md の App Contract を必ず守ってください。\n\n依頼: ${AAP_PROMPT}"
else
  task="既存のアプリを次の依頼どおりに変更してください。既存データを壊さないでください。AGENTS.md の App Contract を守ってください。\n\n依頼: ${AAP_PROMPT}"
fi
exec claude -p "$(printf "$task")" \
  --permission-mode acceptEdits \
  --allowedTools "Read" "Write" "Edit" "Glob" "Grep" "Bash(npm:*)" "Bash(node:*)" "Bash(ls:*)" \
  --max-turns 60
