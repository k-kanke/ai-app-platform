#!/usr/bin/env bash
# The Implementer's patch step (agent-runtime/task-entrypoint.sh) against repositories shaped like the real ones: the stub agent
# changes README.md, node_modules exists (as after `npm ci`), with and without a .gitignore for it. The uploaded patch must contain
# the README change and never node_modules. A fake Control Plane captures the upload. No LLM, no cluster.
#   IMAGE=aap-agent-runtime:dev hack/check-task-entrypoint.sh
# Found the hard way: `git add -- . ':(exclude)node_modules'` exits 1 when node_modules is ignored, so a real task failed with
# "差分を作れませんでした" right after the agent had done its work.
set -euo pipefail
cd "$(dirname "$0")/.."
IMAGE=${IMAGE:-aap-agent-runtime:dev}; PORT=18101
DIR=$(mktemp -d); trap 'kill $SRV 2>/dev/null || true; rm -rf "$DIR"' EXIT
python3 - "$PORT" "$DIR" <<'PY' &
import http.server, sys
port, d = int(sys.argv[1]), sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("content-length") or 0); body = self.rfile.read(n)
        if self.path.endswith("/patch"): open(d + "/patch", "wb").write(body)
        self.send_response(200); self.send_header("content-length", "2"); self.end_headers(); self.wfile.write(b"{}")
    def log_message(self, *a): pass
http.server.HTTPServer(("0.0.0.0", port), H).serve_forever()
PY
SRV=$!; disown $SRV 2>/dev/null || true; sleep 1
run() { # label, gitignore line ("" = none)
  rm -f "$DIR/patch"
  out=$(docker run --rm --user 1000:1000 --entrypoint bash \
    -v "$PWD/agent-runtime/task-entrypoint.sh:/opt/aap/task-entrypoint.sh:ro" \
    -v "$PWD/agent-runtime/agents/task-template.sh:/opt/aap/agents/task-template.sh:ro" \
    -v "$PWD/agent-runtime/AGENTS.task.md:/opt/aap/AGENTS.task.md:ro" \
    -e AAP_AGENT=template -e AAP_TASK_ID=abc123 -e AAP_REPO=k-kanke/x -e AAP_PROMPT="task check" \
    -e AAP_CONTROL_PLANE_URL=http://host.docker.internal:$PORT -e AAP_PROGRESS_TOKEN=t -e HOME=/tmp/home \
    -e GITIGNORE="$2" "$IMAGE" -c 'mkdir -p /workspace/repo && cd /workspace/repo && git init -q && echo hello > README.md
      [ -z "$GITIGNORE" ] || echo "$GITIGNORE" > .gitignore
      mkdir -p node_modules/pkg && echo module.exports=1 > node_modules/pkg/index.js
      git add README.md $( [ -z "$GITIGNORE" ] || echo .gitignore ) && git -c user.name=t -c user.email=t@t commit -q -m init
      /opt/aap/task-entrypoint.sh' 2>&1) && rc=0 || rc=$?
  [ "$rc" -eq 0 ] || { echo "$out" | tail -8; echo "FAIL ($1): the Implementer failed (exit $rc)"; exit 1; }
  [ -s "$DIR/patch" ] || { echo "FAIL ($1): no patch was uploaded"; exit 1; }
  grep -q "^+++ b/README.md" "$DIR/patch" || { echo "FAIL ($1): the README change is not in the patch"; exit 1; }
  ! grep -q "node_modules" "$DIR/patch" || { echo "FAIL ($1): node_modules leaked into the patch"; exit 1; }
  echo "  ok $1: the patch has the README change and no node_modules ($(wc -c < "$DIR/patch") bytes)"
}
run "node_modules is in .gitignore" "node_modules/"
run "no .gitignore at all" ""
