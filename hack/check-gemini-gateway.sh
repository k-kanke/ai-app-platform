#!/usr/bin/env bash
# Regression check for the LLM gateway contract: the REAL Gemini CLI, started the way the Implementer starts it
# (agent-runtime/task-entrypoint.sh + agents/task-gemini.sh), must send its requests to GOOGLE_GEMINI_BASE_URL with the
# task token as its key, using only calls the gateway allows. A fake gateway stands in for the LLM (no API key, no cost).
#   IMAGE=aap-agent-runtime:gemini hack/check-gemini-gateway.sh     # an image built with INSTALL_GEMINI=true
# With the real gateway binary between the CLI and the fake LLM (checks the proxy against the CLI's real traffic: SSE,
# query strings, and that the task token is swapped for the provider key):
#   WITH_GATEWAY=1 IMAGE=aap-agent-runtime:gemini hack/check-gemini-gateway.sh
# Found the hard way: without security.auth.selectedType the CLI stops with "Invalid auth method selected."
set -euo pipefail
cd "$(dirname "$0")/.."
IMAGE=${IMAGE:-aap-agent-runtime:gemini}
LOG=$(mktemp); PORT=18099; GWPORT=18097; REALKEY=REAL-PROVIDER-KEY; SECRET=check-secret
BASEURL=http://host.docker.internal:$PORT; KEY=abc123.9999999999.sig
python3 - "$PORT" "$LOG" <<'PY' &
import http.server, json, sys
port, log = int(sys.argv[1]), sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def note(self, s):
        open(log, "a").write(s + "\n")
    def do_POST(self):
        n = int(self.headers.get("content-length") or 0); self.rfile.read(n)
        self.note("POST %s key=%s" % (self.path, self.headers.get("x-goog-api-key")))
        body = json.dumps({"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP","index":0}],
                           "usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}})
        if self.path.startswith("/internal/"):
            out, ctype = b"{}", "application/json"
        elif ":streamGenerateContent" in self.path:   # the main turn streams: server-sent events
            out, ctype = ("data: " + body + "\n\n").encode(), "text/event-stream"
        else:
            out, ctype = body.encode(), "application/json"
        self.send_response(200); self.send_header("content-type", ctype); self.send_header("content-length", str(len(out))); self.end_headers(); self.wfile.write(out)
    def log_message(self, *a): pass
http.server.HTTPServer(("0.0.0.0", port), H).serve_forever()
PY
SRV=$!; trap 'kill $SRV ${GW:-} 2>/dev/null || true' EXIT; sleep 1
if [ -n "${WITH_GATEWAY:-}" ]; then
  ( cd control-plane && go build -o "$LOG.gw" ./cmd/llm-gateway )
  KEY=$(cd control-plane && go run ./hack/mint-llm-token "$SECRET" abc123 1h)
  AAP_TOKEN_SECRET=$SECRET GEMINI_API_KEY=$REALKEY LLM_UPSTREAM=http://127.0.0.1:$PORT LLM_LISTEN=:$GWPORT "$LOG.gw" >"$LOG.gw.log" 2>&1 &
  GW=$!; sleep 1
  BASEURL=http://host.docker.internal:$GWPORT
fi
out=$(docker run --rm --user 1000:1000 --entrypoint bash \
  -v "$PWD/agent-runtime/task-entrypoint.sh:/opt/aap/task-entrypoint.sh:ro" \
  -v "$PWD/agent-runtime/agents/task-gemini.sh:/opt/aap/agents/task-gemini.sh:ro" \
  -v "$PWD/agent-runtime/AGENTS.task.md:/opt/aap/AGENTS.task.md:ro" \
  -e AAP_AGENT=gemini -e AAP_TASK_ID=abc123 -e AAP_REPO=k-kanke/x -e AAP_PROMPT="say hi" \
  -e AAP_CONTROL_PLANE_URL=http://host.docker.internal:$PORT -e AAP_PROGRESS_TOKEN=progress \
  -e GOOGLE_GEMINI_BASE_URL=$BASEURL -e GEMINI_API_KEY=$KEY \
  -e GEMINI_CLI_TRUST_WORKSPACE=true -e HOME=/tmp/home \
  "$IMAGE" -c 'mkdir -p /workspace/repo && cd /workspace/repo && git init -q && git -c user.name=t -c user.email=t@t commit -q --allow-empty -m init && echo hi > a.txt && git add a.txt && git -c user.name=t -c user.email=t@t commit -q -m a && /opt/aap/task-entrypoint.sh' 2>&1) || { echo "$out" | tail -15; echo FAIL: the Implementer entrypoint failed; exit 1; }
echo "$out" | tail -3
grep -q "Invalid auth method" <<<"$out" && { echo "FAIL: the CLI rejected the auth method"; exit 1; }
if [ -n "${WITH_GATEWAY:-}" ]; then
  # behind the gateway the fake LLM must only ever see the provider key, never the task token
  n=$(grep -cE "^POST /v1beta/models/gemini-[A-Za-z0-9._-]+:(stream)?[Gg]enerateContent(\?[^ ]*)? key=$REALKEY$" "$LOG" || true)
  [ "$n" -ge 1 ] || { echo "FAIL: nothing reached the provider through the gateway with the provider key"; cat "$LOG" "$LOG.gw.log"; exit 1; }
  ! grep -q "key=abc123" "$LOG" || { echo "FAIL: the task token leaked to the provider"; exit 1; }
  echo "  ok through the real gateway: $n call(s) reached the provider with the PROVIDER key, none with the task token"
  grep -q "llm request" "$LOG.gw.log" && echo "  ok the gateway logged the requests ($(grep -c 'llm request' "$LOG.gw.log"))"
else
  n=$(grep -cE "^POST /v1beta/models/gemini-[A-Za-z0-9._-]+:(stream)?[Gg]enerateContent(\?[^ ]*)? key=abc123.9999999999.sig$" "$LOG" || true)
  [ "$n" -ge 1 ] || { echo "FAIL: no generateContent call reached the gateway with the task token"; cat "$LOG"; exit 1; }
  # the gateway decides on the path only (a query such as ?alt=sse is passed on, and a key= query is dropped)
  bad=$(grep "^POST /v1" "$LOG" | sed -E 's/\?[^ ]* key=/ key=/' | grep -vE "^POST /v1(beta)?/models/gemini-[A-Za-z0-9._-]+:(generateContent|streamGenerateContent|countTokens|embedContent|batchEmbedContents) key=" || true)
  [ -z "$bad" ] || { echo "FAIL: the CLI made a call the gateway would refuse:"; echo "$bad"; exit 1; }
  echo "  ok the CLI sent $n generate call(s) to the gateway with the task token, and only allowed calls"
fi
grep -q "^POST /internal/v1/tasks/abc123/events key=None" "$LOG" && echo "  ok progress events reached the Control Plane stand-in"
echo "  models used: $(grep -oE "models/[^:]+" "$LOG" | sort -u | tr '\n' ' ')"
