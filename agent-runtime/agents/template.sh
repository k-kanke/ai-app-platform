#!/bin/bash
# Offline stub agent: deterministic, no LLM. Lets the whole platform flow be
# exercised (and tested) without API keys. NOT a substitute for a real agent.
set -euo pipefail
cd /workspace
prompt="${AAP_PROMPT:-}"

if [[ "$prompt" == *FAIL_QUOTA* ]]; then
  # Mimics Gemini CLI hitting the project's spending cap.
  echo "Attempt 1 failed with status 429. Your project has exceeded its monthly spending cap." >&2; exit 1
fi
if [[ "$prompt" == *FAIL_AGENT* ]]; then
  echo "stub agent: simulated failure"; exit 1
fi

if [ "${AAP_OP_KIND}" = "create" ]; then
  if [[ "$prompt" =~ (ご飯|食事|昼|夜|予定表|meal) ]]; then tpl=meal; else tpl=generic; fi
  echo "stub agent: scaffolding template '$tpl'"
  cp -a "/opt/aap/templates/$tpl/." /workspace/
  printf '%s\n' "$prompt" > REQUEST.txt
else
  echo "stub agent: recording change request (no code understanding in stub)"
  printf -- '- %s\n' "$prompt" >> CHANGES.md
  if [[ "$prompt" == *BREAK_APP* ]]; then
    # Simulates an agent that produces broken code (passes no health check).
    echo 'process.exit(1)' > server.js
  fi
fi
