#!/usr/bin/env python3
"""End-to-end check AND measurement of the release strategy on a throw-away kind cluster.

  SETUP_ONLY=1 hack/e2e-kind.sh        # fresh kind-aap-dev with the platform deployed (stub agent, no LLM cost)
  hack/release-e2e.py                  # run + measure; writes docs/measurements/data/release-e2e-<date>.json

It never touches any kube context other than kind-aap-dev.

What is measured (all with the offline stub agent, so only the platform's own cost shows up):
  E1  does the family SEE a broken state while a change is being made?     (in-place vs release strategy)
  T1  create -> preview ready; modify -> preview ready
  T2  approve: request -> live, and the production downtime while it switches
  T3  rollback (with data): request -> done, and the downtime
  D1  preview data is a COPY of production data (writes to it never reach production)
  D2  rollback with data restores the data snapshot taken just before the release went live
  R1  the live release is read-only and its content hash differs from the next one
"""
import json, os, subprocess, sys, time, urllib.request, urllib.error, datetime as dt

CTX = "kind-aap-dev"
NS = "aap-apps"
# The test pod lives where production traffic is allowed to come from (see runtime-ingress): ingress-nginx.
TNS = "ingress-nginx"
API = "http://127.0.0.1:18080"
OUT = os.path.join(os.path.dirname(__file__), "..", "docs", "measurements", "data",
                   "release-e2e-%s.json" % dt.date.today().isoformat())
results = {"startedAt": dt.datetime.now(dt.timezone.utc).isoformat(), "checks": [], "measurements": {}}


def sh(*a, check=True, inp=None, timeout=120):
    r = subprocess.run(list(a), capture_output=True, text=True, input=inp, timeout=timeout)
    if check and r.returncode != 0:
        raise RuntimeError("%s -> %s" % (" ".join(a)[:150], (r.stderr or r.stdout)[-400:]))
    return r.stdout


def k(*a, **kw):
    return sh("kubectl", "--context", CTX, *a, **kw)


def api(method, path, body=None):
    req = urllib.request.Request(API + path, method=method, data=json.dumps(body).encode() if body is not None else None,
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def check(name, ok, detail=""):
    results["checks"].append({"name": name, "ok": bool(ok), "detail": str(detail)})
    print("  %s %s%s" % ("ok  " if ok else "FAIL", name, (" -- " + str(detail)) if detail and not ok else ""))
    if not ok:
        finish(1)


def finish(code):
    results["finishedAt"] = dt.datetime.now(dt.timezone.utc).isoformat()
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    json.dump(results, open(OUT, "w"), ensure_ascii=False, indent=1)
    print("\nraw results ->", os.path.relpath(OUT))
    sys.exit(code)


def say(t):
    print("\n== " + t)


def wait_for(fn, timeout=240, every=0.5, what=""):
    t0 = time.time()
    while time.time() - t0 < timeout:
        try:
            v = fn()
        except (urllib.error.URLError, ConnectionError, TimeoutError):
            v = None  # e.g. the port-forward is not up yet
        if v:
            return v, time.time() - t0
        time.sleep(every)
    raise RuntimeError("timed out waiting for " + what)


def app(id_):
    c, d = api("GET", "/api/v1/apps/" + id_)
    return d if c == 200 else {}


def settled(id_, phase, draft=None):
    """Phase reached and the operation finished (so the next call cannot race it)."""
    def f():
        d = app(id_)
        op = d.get("operation") or {}
        if d.get("phase") == phase and op.get("state") in ("SUCCEEDED", "FAILED") and (draft is None or (d.get("draftState") or "") == draft):
            return d
    return f


# ---- in-cluster helper pod: calls apps and runs the production probe ---------------------------------
PROBE_JS = r"""
const http=require('http');const [,,url,marker,out]=process.argv;const fs=require('fs');
function hit(){const t=Date.now();const req=http.get(url,{timeout:700},res=>{let b='';res.on('data',d=>b+=d);
 res.on('end',()=>log(t,res.statusCode,b.includes(marker)?'ok':'BROKEN'));});
 req.on('timeout',()=>{req.destroy();log(t,0,'timeout')});req.on('error',()=>log(t,0,'error'));}
function log(t,c,m){fs.appendFileSync(out,t+' '+c+' '+m+'\n');}
setInterval(hit,200);
"""


def tools_up():
    k("create", "namespace", TNS, check=False)
    k("-n", TNS, "delete", "pod", "tools", "--ignore-not-found", "--wait=true")
    k("apply", "-f", "-", inp="""apiVersion: v1
kind: Pod
metadata: {name: tools, namespace: %s}
spec:
  automountServiceAccountToken: false
  containers:
    - {name: t, image: aap-app-runtime:dev, imagePullPolicy: IfNotPresent, command: ["sleep","3600"]}
""" % TNS)
    k("-n", TNS, "wait", "--for=condition=Ready", "pod/tools", "--timeout=90s")
    k("-n", TNS, "exec", "-i", "tools", "--", "sh", "-c", "cat > /tmp/probe.js", inp=PROBE_JS)


REACH_LAG = []  # seconds between "the platform says Ready" and "the URL actually answers"


def call(path_url, method="GET", body=None, retry_s=12):
    """HTTP from inside the cluster (the apps are only reachable there).
    Right after a pod becomes Ready, the Service route may lag by a moment (eventual consistency), so a
    refused connection is retried for a few seconds and the wait is recorded."""
    t0, first = time.time(), True
    while True:
        try:
            out = _call_once(path_url, method, body)
            if first is False:
                REACH_LAG.append(round(time.time() - t0, 2))
            return out
        except RuntimeError as e:
            first = False
            if time.time() - t0 > retry_s or ("ECONNREFUSED" not in str(e) and "EAI_AGAIN" not in str(e) and "fetch failed" not in str(e)):
                raise
            time.sleep(0.25)


def _call_once(path_url, method="GET", body=None):
    js = ("fetch(%s,{method:%s,headers:{'Content-Type':'application/json'},body:%s}).then(async r=>console.log(r.status+' '+await r.text()))"
          % (json.dumps(path_url), json.dumps(method), json.dumps(json.dumps(body)) if body is not None else "undefined"))
    out = k("-n", TNS, "exec", "tools", "--", "node", "-e", js).strip()
    code, _, text = out.partition(" ")
    return int(code), text


def probe_start(url, marker, name):
    k("-n", TNS, "exec", "tools", "--", "sh", "-c", "rm -f /tmp/%s.log; nohup node /tmp/probe.js %s %s /tmp/%s.log >/dev/null 2>&1 &" % (name, url, marker, name))
    time.sleep(1.0)


def probe_stop(name):
    k("-n", TNS, "exec", "tools", "--", "sh", "-c", "pkill -f 'probe.js.*%s' || true" % name, check=False)
    raw = k("-n", TNS, "exec", "tools", "--", "cat", "/tmp/%s.log" % name)
    rows = [l.split() for l in raw.strip().splitlines() if l.strip()]
    return [(int(t) / 1000.0, int(c), m) for t, c, m in rows]


def analyse(rows):
    """From the probe samples: broken-page time, error time, and the longest run of failed samples."""
    if not rows:
        return {"samples": 0}
    broken = [r for r in rows if r[2] == "BROKEN"]
    bad = [r for r in rows if r[2] in ("timeout", "error") or (r[1] >= 500)]
    longest, run_start, last = 0.0, None, None
    for t, c, m in rows:
        failed = m != "ok"
        if failed and run_start is None:
            run_start = t
        if not failed and run_start is not None:
            longest = max(longest, t - run_start); run_start = None
        last = t
    if run_start is not None:
        longest = max(longest, last - run_start)
    span = rows[-1][0] - rows[0][0]
    return {"samples": len(rows), "span_s": round(span, 1), "broken_samples": len(broken), "error_samples": len(bad),
            "broken_seconds": round(len(broken) * 0.2, 1), "longest_unavailable_s": round(longest, 2)}


def main():
    sh("kind", "get", "clusters")  # fails early if kind is missing
    if "aap-dev" not in sh("kind", "get", "clusters").split():
        sys.exit("cluster aap-dev not found; run: SETUP_ONLY=1 hack/e2e-kind.sh")
    subprocess.Popen(["kubectl", "--context", CTX, "-n", "platform-system", "port-forward", "svc/control-plane", "18080:8080"],
                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    wait_for(lambda: api("GET", "/healthz")[0] == 200, 30, 1, "control plane")
    tools_up()
    for leftover in ("rel", "inp"):  # re-runs on the same cluster start clean
        if api("GET", "/api/v1/apps/" + leftover)[0] == 200:
            api("DELETE", "/api/v1/apps/%s?purge=true" % leftover)
            wait_for(lambda l=leftover: api("GET", "/api/v1/apps/" + l)[0] == 404, 90, 1, "cleanup of " + leftover)
    time.sleep(8)
    M = results["measurements"]

    say("1. release strategy: create stops at a PREVIEW; nothing is live yet")
    t0 = time.time()
    c, _ = api("POST", "/api/v1/apps", {"id": "rel", "name": "リリース方式", "prompt": "家族の昼ご飯と夜ご飯の予定表", "strategy": "release"})
    check("create accepted", c == 202, c)
    d, _ = wait_for(settled("rel", "PREVIEW", "PREVIEW_READY"), 240, 1, "preview")
    M["create_to_preview_s"] = round(time.time() - t0, 1)
    check("app is in PREVIEW with a previewUrl-able draft", d["draftState"] == "PREVIEW_READY" and d["liveRelease"] == 0)
    check("no production Deployment exists yet", k("-n", NS, "get", "deploy", "aap-gen-rel", check=False).strip() == "" )
    check("preview Deployment runs draft/ read-only on its own data",
          "draft" in k("-n", NS, "get", "deploy", "aap-gen-rel-preview", "-o", "jsonpath={.spec.template.metadata.annotations}")
          and "aap-gen-rel-pdata" in k("-n", NS, "get", "deploy", "aap-gen-rel-preview", "-o", "jsonpath={.spec.template.spec.volumes[*].persistentVolumeClaim.claimName}"))
    code, _ = call("http://aap-gen-rel-preview.aap-apps.svc/healthz")
    check("preview answers /healthz", code == 200, code)

    say("2. approve -> release 1 is live (read-only, content hash)")
    t0 = time.time()
    c, _ = api("POST", "/api/v1/apps/rel/approve")
    check("approve accepted", c == 202, c)
    d, _ = wait_for(settled("rel", "READY"), 240, 0.5, "release 1")
    M["first_approve_s"] = round(time.time() - t0, 1)
    check("release 1 live", d["liveRelease"] == 1)
    ann = k("-n", NS, "get", "deploy", "aap-gen-rel", "-o", "jsonpath={.spec.template.metadata.annotations.aap\\.dev/source}")
    check("production runs releases/1", ann == "releases/1", ann)
    pod = k("-n", NS, "get", "pod", "-l", "aap.dev/app-id=rel,aap.dev/role=runtime", "-o", "jsonpath={.items[0].metadata.name}")
    ro = k("-n", NS, "exec", pod, "--", "sh", "-c", "echo x >> /app/server.js 2>&1; echo rc=$?", check=False)
    check("the live release is read-only inside the pod", "rc=0" not in ro, ro.strip())
    _, rels = api("GET", "/api/v1/apps/rel/releases")
    h1 = rels["releases"][0]["sourceHash"]
    check("release 1 has a 64-hex content hash", len(h1) == 64, h1)
    code, _ = call("http://aap-gen-rel.aap-apps.svc/healthz")
    check("production answers /healthz", code == 200, code)

    say("3. production data, then a modification: the preview gets a COPY, production is untouched")
    prod_ans = {"member": "母", "date": "2026-10-05", "slot": "dinner", "value": "yes"}
    call("http://aap-gen-rel.aap-apps.svc/api/meals", "PUT", prod_ans)
    t0 = time.time()
    api("POST", "/api/v1/apps/rel/changes", {"prompt": "文字を大きくして"})
    d, _ = wait_for(settled("rel", "READY", "PREVIEW_READY"), 240, 0.5, "preview of the change")
    M["modify_to_preview_s"] = round(time.time() - t0, 1)
    ann = k("-n", NS, "get", "deploy", "aap-gen-rel", "-o", "jsonpath={.spec.template.metadata.annotations.aap\\.dev/source}")
    check("production still runs releases/1 while the change is only a preview", ann == "releases/1", ann)
    _, txt = call("http://aap-gen-rel-preview.aap-apps.svc/api/meals")
    check("D1: preview data contains the production answer (it is a copy)", "2026-10-05|dinner|母" in txt, txt[:120])
    call("http://aap-gen-rel-preview.aap-apps.svc/api/meals", "PUT", {"member": "父", "date": "2026-10-06", "slot": "lunch", "value": "no"})
    _, txt = call("http://aap-gen-rel.aap-apps.svc/api/meals")
    check("D1: a write to the preview never reaches production", "2026-10-06|lunch|父" not in txt, txt[:120])

    say("4. E1: a BROKEN change, with production probed every 200 ms (release strategy)")
    probe_start("http://aap-gen-rel.aap-apps.svc/", "ご飯", "p_rel")
    time.sleep(2)
    api("POST", "/api/v1/apps/rel/changes", {"prompt": "BREAK_UI 画面を作り直して"})
    wait_for(settled("rel", "READY"), 240, 0.5, "broken change to finish")
    time.sleep(2)
    m_rel = analyse(probe_stop("p_rel"))
    M["E1_release"] = m_rel
    check("E1: production never showed the broken page (release strategy)", m_rel["broken_samples"] == 0 and m_rel["error_samples"] == 0, m_rel)
    d = app("rel")
    check("the failed change left no preview and production on release 1", d["liveRelease"] == 1 and not d.get("draftState"), d)

    say("5. a good change -> approve -> release 2, with production probed (downtime) and data snapshot")
    api("POST", "/api/v1/apps/rel/changes", {"prompt": "文字を大きくして(2回目)"})
    wait_for(settled("rel", "READY", "PREVIEW_READY"), 240, 0.5, "preview 2")
    call("http://aap-gen-rel.aap-apps.svc/api/meals", "PUT", {"member": "妹", "date": "2026-10-07", "slot": "lunch", "value": "yes"})  # marker X (before release 2)
    probe_start("http://aap-gen-rel.aap-apps.svc/healthz", "ok", "p_app")
    t0 = time.time()
    api("POST", "/api/v1/apps/rel/approve")
    wait_for(settled("rel", "READY"), 240, 0.5, "release 2")
    M["approve_to_live_s"] = round(time.time() - t0, 1)
    time.sleep(2)
    m_app = analyse(probe_stop("p_app"))
    M["approve_switch"] = m_app
    d = app("rel")
    check("release 2 live", d["liveRelease"] == 2)
    _, rels = api("GET", "/api/v1/apps/rel/releases")
    h2 = rels["releases"][0]["sourceHash"]
    check("R1: release 2 has a different content hash from release 1", h2 != h1, (h1[:12], h2[:12]))
    check("release 2 recorded a data snapshot", rels["releases"][0]["dataSnapshot"] is True)
    print("     approve -> live %.1fs ; longest unavailable window %.2fs (samples %d)" % (M["approve_to_live_s"], m_app["longest_unavailable_s"], m_app["samples"]))

    say("6. rollback WITH data: source back to release 1 and the data back to the snapshot")
    call("http://aap-gen-rel.aap-apps.svc/api/meals", "PUT", {"member": "孝太郎", "date": "2026-10-08", "slot": "dinner", "value": "no"})  # marker Y (after release 2)
    probe_start("http://aap-gen-rel.aap-apps.svc/healthz", "ok", "p_rb")
    t0 = time.time()
    c, _ = api("POST", "/api/v1/apps/rel/rollback", {"withData": True})
    check("rollback accepted", c == 202, c)
    wait_for(settled("rel", "READY"), 240, 0.5, "rollback")
    M["rollback_s"] = round(time.time() - t0, 1)
    time.sleep(2)
    m_rb = analyse(probe_stop("p_rb"))
    M["rollback_switch"] = m_rb
    d = app("rel")
    check("production is back on release 1", d["liveRelease"] == 1)
    ann = k("-n", NS, "get", "deploy", "aap-gen-rel", "-o", "jsonpath={.spec.template.metadata.annotations.aap\\.dev/source}")
    check("production runs releases/1 again", ann == "releases/1", ann)
    _, txt = call("http://aap-gen-rel.aap-apps.svc/api/meals")
    check("D2: data written before release 2 survived (marker X)", "2026-10-07|lunch|妹" in txt, txt[:160])
    check("D2: data written after release 2 is gone (marker Y)", "2026-10-08|dinner|孝太郎" not in txt, txt[:160])
    print("     rollback %.1fs ; longest unavailable window %.2fs" % (M["rollback_s"], m_rb["longest_unavailable_s"]))

    say("7. control: the same broken change on an IN-PLACE app (today's behaviour)")
    api("POST", "/api/v1/apps", {"id": "inp", "name": "従来方式", "prompt": "家族の昼ご飯と夜ご飯の予定表", "strategy": "inplace"})
    wait_for(settled("inp", "READY"), 240, 1, "in-place app")
    probe_start("http://aap-gen-inp.aap-apps.svc/", "ご飯", "p_inp")
    time.sleep(2)
    api("POST", "/api/v1/apps/inp/changes", {"prompt": "BREAK_UI 画面を作り直して"})
    wait_for(settled("inp", "READY"), 300, 0.5, "in-place broken change to finish")
    time.sleep(2)
    m_inp = analyse(probe_stop("p_inp"))
    M["E1_inplace"] = m_inp
    print("     in-place: broken page served for about %.1fs (%d samples)" % (m_inp["broken_seconds"], m_inp["broken_samples"]))
    check("E1: the in-place strategy DID show the broken page to its users (the problem this solves)", m_inp["broken_samples"] > 0, m_inp)

    M["reachability_lag_s"] = REACH_LAG
    say("summary")
    if REACH_LAG:
        print("  Ready -> reachable lag seen %d time(s): %s s" % (len(REACH_LAG), REACH_LAG))
    print("  E1 broken page seen by users   in-place: %.1fs   release: %.1fs" % (m_inp["broken_seconds"], m_rel["broken_seconds"]))
    print("  create -> preview %.1fs | modify -> preview %.1fs | approve -> live %.1fs (unavailable %.2fs) | rollback %.1fs (unavailable %.2fs)" % (
        M["create_to_preview_s"], M["modify_to_preview_s"], M["approve_to_live_s"], m_app["longest_unavailable_s"], M["rollback_s"], m_rb["longest_unavailable_s"]))
    finish(0)


if __name__ == "__main__":
    try:
        main()
    finally:
        subprocess.run(["pkill", "-f", "port-forward svc/control-plane 18080"], capture_output=True)
