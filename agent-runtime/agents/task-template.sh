#!/bin/bash
# Offline stub for repository tasks: deterministic, no LLM, no network. Appends one line to README.md so the
# whole task flow (clone -> patch -> upload -> approval) can be exercised without API keys.
# Special prompts: FAIL_TASK exits 1; NO_CHANGE touches nothing.
set -euo pipefail
cd /workspace/repo
prompt="${AAP_PROMPT:-}"
case "$prompt" in
  *FAIL_TASK*) echo "stub agent: failing on request" >&2; exit 1 ;;
  *NO_CHANGE*) echo "stub agent: leaving the repository untouched"; exit 0 ;;
esac
line=$(printf '%s' "$prompt" | tr '\n' ' ' | cut -c1-120)
printf '\n<!-- stub agent: %s -->\n' "$line" >> README.md
echo "stub agent: appended a line to README.md"
