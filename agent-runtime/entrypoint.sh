#!/bin/bash
# Agent Job entrypoint. Runs one create/modify request against /workspace.
# Success/failure is decided by this process's exit code (Job status), the
# progress events below are advisory for the UI only.
set -uo pipefail

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
"/opt/aap/agents/${AAP_AGENT}.sh" || die GENERATING "アプリを作る途中で止まりました"

# --- App Contract checks (platform-owned, the Agent cannot skip them) ---
[ -f package.json ] || die GENERATING "package.json がありません"
node -e 'const p=require("./package.json"); process.exit(p.scripts&&p.scripts.start?0:1)' \
  || die GENERATING "npm start が定義されていません"

if node -e 'const p=require("./package.json"); process.exit(Object.keys(p.dependencies||{}).length?0:1)'; then
  post_event GENERATING "部品を準備しています"
  npm install --omit=dev --no-audit --no-fund || die GENERATING "部品の準備に失敗しました"
fi

if node -e 'const p=require("./package.json"); process.exit(p.scripts&&p.scripts.test?0:1)'; then
  post_event TESTING "動作を確認しています"
  npm test || die TESTING "確認テストに失敗しました"
fi
post_event TESTING "確認できました"
echo "== done"
