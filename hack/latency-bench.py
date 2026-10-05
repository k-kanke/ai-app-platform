#!/usr/bin/env python3
"""Baseline latency benchmark for the edit cycle (docs/deep-dive-inner-loop-latency.md §5-§6, ML1).

Runs a fixed set of modification requests against ONE throw-away app ("bench") on the home cluster,
restoring its source to the same starting state before every run, and records every trial.
Everything is measured by the agent trace + the Control Plane's stored events (see latency-report.py).

  hack/latency-bench.py --ssh ubuntu-26 --setup                   # create "bench" once and freeze its baseline source
  hack/latency-bench.py --ssh ubuntu-26 --run --reps 2            # B1..B5 x reps, interleaved
  hack/latency-bench.py --ssh ubuntu-26 --run --plan B1 --reps 2  # a subset
  hack/latency-bench.py --ssh ubuntu-26 --teardown                # delete "bench" (with its data)

Safety:
  * The only app this script touches is "bench". The reset Job mounts bench's two PVCs only; it refuses to
    run unless both PVCs exist and carry the label aap.dev/app-id=bench.
  * Every run costs real LLM money (about 20-30 JPY). --max-runs is a hard stop, and the script stops at the
    first "AI usage limit" failure.
  * Raw trials are appended to the --out file after every run, so an interrupted benchmark loses nothing.
"""
import argparse, importlib.util, json, shlex, statistics, subprocess, sys, textwrap, time, datetime as dt, os

APP = "bench"  # hard-coded on purpose
NS = "aap-apps"

BASE_PROMPT = ("毎晩洗濯物を干すのだが、夜に降るのか、明日降るのか毎回調べるのが面倒なので教えてくれるようにしてほしい。\n"
               "ちなみに八王子市に住んでるので八王子市だけで良いです")

PROMPTS = {
    "B1": "文字をもっと大きくして",
    "B2": "更新ボタンの色を青に変えて、押したときに少し動くようにして",
    "B3": "洗濯物を干す時刻を設定できる入力欄を足して、次に開いたときも覚えているようにして",
    "B4": "天気が取得できなかったときの表示を、もっと分かりやすくして",
    "B5": "明日の朝の予報も見られるようにして",
}


def load_report():
    here = os.path.dirname(os.path.abspath(__file__))
    spec = importlib.util.spec_from_file_location("latency_report", os.path.join(here, "latency-report.py"))
    m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
    return m


def ssh(host, cmd, stdin=None, timeout=300):
    r = subprocess.run(["ssh", "-o", "BatchMode=yes", host, cmd], input=stdin, capture_output=True, text=True, timeout=timeout)
    return r.returncode, r.stdout, r.stderr


NODE = ("let b='';process.stdin.on('data',d=>b+=d).on('end',async()=>{const [m,p]=process.argv.slice(1);"
        "const r=await fetch('http://control-plane.platform-system.svc:8080'+p,{method:m,headers:{'Content-Type':'application/json'},body:b||undefined});"
        "console.log(r.status+' '+await r.text())})")


def cp(host, method, path, body=None):
    """Call the Control Plane API through the portal pod (same route the Portal uses)."""
    remote = "kubectl -n platform-system exec -i deploy/portal -- node -e %s %s %s" % (
        shlex.quote(NODE), shlex.quote(method), shlex.quote(path))
    rc, out, err = ssh(host, remote, stdin=json.dumps(body) if body is not None else "")
    if rc != 0:
        raise RuntimeError("cp call failed: " + (err or out)[-300:])
    code, _, text = out.strip().partition(" ")
    try:
        return int(code), json.loads(text) if text else {}
    except ValueError:
        return int(code), {"raw": text}


def guard_pvcs(host):
    """Refuse to touch anything that is not bench's own volumes."""
    rc, out, err = ssh(host, "kubectl -n %s get pvc -l aap.dev/app-id=%s -o jsonpath='{range .items[*]}{.metadata.name}{\"\\n\"}{end}'" % (NS, APP))
    names = sorted(out.split())
    want = ["aap-gen-%s-data" % APP, "aap-gen-%s-src" % APP]
    if names != want:
        sys.exit("refusing to continue: expected exactly PVCs %s labelled aap.dev/app-id=%s, found %s" % (want, APP, names))


def helper_job(host, script, name):
    guard_pvcs(host)
    manifest = textwrap.dedent("""
        apiVersion: batch/v1
        kind: Job
        metadata: {name: %(name)s, namespace: %(ns)s, labels: {aap.dev/bench-helper: "true"}}
        spec:
          backoffLimit: 0
          ttlSecondsAfterFinished: 300
          template:
            spec:
              restartPolicy: Never
              automountServiceAccountToken: false
              containers:
                - name: r
                  image: busybox:1.37
                  command: ["sh", "-c", %(script)s]
                  securityContext:
                    allowPrivilegeEscalation: false
                    capabilities: {drop: ["ALL"], add: ["CHOWN", "FOWNER", "DAC_OVERRIDE"]}
                  volumeMounts: [{name: src, mountPath: /src}, {name: data, mountPath: /data}]
              volumes:
                - {name: src, persistentVolumeClaim: {claimName: aap-gen-%(app)s-src}}
                - {name: data, persistentVolumeClaim: {claimName: aap-gen-%(app)s-data}}
    """) % {"name": name, "ns": NS, "app": APP, "script": json.dumps(script)}
    rc, out, err = ssh(host, "kubectl apply -f -", stdin=manifest)
    if rc != 0:
        raise RuntimeError("could not create helper job: " + err[-300:])
    rc, out, err = ssh(host, "kubectl -n %s wait --for=condition=complete job/%s --timeout=120s" % (NS, name))
    if rc != 0:
        raise RuntimeError("helper job did not complete: " + (err or out)[-300:])


FREEZE = "set -e; rm -rf /src/baseline; mkdir -p /src/baseline; cp -a /src/current/. /src/baseline/"
RESET = ("set -e; test -d /src/baseline; find /src/current -mindepth 1 -delete; cp -a /src/baseline/. /src/current/; "
         "chown -R 1000:1000 /src/current; find /data -mindepth 1 -delete")


def wait_op(host, op_id, timeout=900, poll=6):
    t0 = time.time()
    while time.time() - t0 < timeout:
        code, d = cp(host, "GET", "/api/v1/apps/%s/operations" % APP)
        for o in d.get("operations", []):
            if o["id"] == op_id and o["state"] in ("SUCCEEDED", "FAILED"):
                return o
        time.sleep(poll)
    raise RuntimeError("operation %s did not finish within %ds" % (op_id, timeout))


def wait_app_ready(host, timeout=600):
    t0 = time.time()
    while time.time() - t0 < timeout:
        code, d = cp(host, "GET", "/api/v1/apps/%s" % APP)
        op = d.get("operation") or {}
        if d.get("phase") == "READY" and op.get("state") == "SUCCEEDED":
            return d
        if d.get("phase") == "FAILED":
            raise RuntimeError("bench app creation failed: %s" % op.get("userMessage"))
        time.sleep(6)
    raise RuntimeError("bench app was not ready in time")


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def do_setup(a):
    code, d = cp(a.ssh, "GET", "/api/v1/apps/%s" % APP)
    if code == 200:
        print("bench already exists (phase %s); freezing its current source as the baseline" % d.get("phase"))
    else:
        print("creating bench (one LLM run, about 30 JPY)...")
        code, d = cp(a.ssh, "POST", "/api/v1/apps", {"id": APP, "name": "計測用", "prompt": BASE_PROMPT})
        if code not in (200, 202):
            sys.exit("create failed: %s %s" % (code, d))
        wait_app_ready(a.ssh)
    helper_job(a.ssh, FREEZE, "bench-freeze-%d" % int(time.time()))
    print("baseline frozen: /src/baseline = the source of the app as created")


def do_run(a):
    rep = load_report()
    plan = [x for x in a.plan.split(",") if x]
    for p in plan:
        if p not in PROMPTS:
            sys.exit("unknown prompt %s (have %s)" % (p, ",".join(PROMPTS)))
    order = [(r, p) for r in range(a.reps) for p in plan]
    if len(order) > a.max_runs:
        sys.exit("plan has %d runs but --max-runs is %d" % (len(order), a.max_runs))
    code, d = cp(a.ssh, "GET", "/api/v1/apps/%s" % APP)
    if code != 200 or d.get("phase") != "READY":
        sys.exit("bench is not READY; run --setup first")
    est = len(order) * 25
    print("plan: %d runs (%s x %d), estimated cost about %d JPY (20-30 JPY per run)" % (len(order), ",".join(plan), a.reps, est))
    if a.dry_run:
        return
    trials = []
    out = json.load(open(a.out)) if os.path.exists(a.out) else {"label": a.label, "protocol": {}, "trials": []}
    out["label"] = a.label or out.get("label", "")
    out["protocol"] = {"app": APP, "plan": plan, "reps": a.reps, "prompts": {k: PROMPTS[k] for k in plan},
                       "reset": "restore source from baseline + wipe data before every run", "settle_s": rep.SETTLE_S}
    for i, (r, p) in enumerate(order, 1):
        print("[%d/%d] reset..." % (i, len(order)), end=" ", flush=True)
        helper_job(a.ssh, RESET, "bench-reset-%d" % int(time.time()))
        t_req = now()
        code, d = cp(a.ssh, "POST", "/api/v1/apps/%s/changes" % APP, {"prompt": PROMPTS[p]})
        if code != 202:
            sys.exit("change request failed: %s %s" % (code, d))
        op_id = d["operation"]["id"]
        print("%s (op %s) running..." % (p, op_id[:8]), end=" ", flush=True)
        op = wait_op(a.ssh, op_id)
        trial = {"i": i, "rep": r, "label": p, "op": op_id, "requested_at": t_req, "state": op["state"],
                 "userMessage": op.get("userMessage", "")}
        out["trials"].append(trial)
        json.dump(out, open(a.out, "w"), ensure_ascii=False, indent=1)
        print(op["state"])
        if op["state"] != "SUCCEEDED":
            print("  stopped: %s" % (op.get("userMessage") or "failed"))
            if "利用上限" in (op.get("userMessage") or ""):
                break
        # let the runtime settle and give the node a moment before the next trial
        wait_app_ready(a.ssh)
        time.sleep(a.pause)
    # final: fetch operations + events and attach them, then print the report
    apps = rep.fetch_via_ssh(a.ssh)
    out["fetchedAt"] = now(); out["apps"] = {APP: apps[APP]}
    json.dump(out, open(a.out, "w"), ensure_ascii=False, indent=1)
    print("\nraw data ->", a.out)
    summarize(out, rep)


def summarize(data, rep):
    labels = {t["op"]: t["label"] for t in data["trials"]}
    rows = [r for r in rep.build_rows(data["apps"]) if r["op"] in labels]
    for r in rows:
        r["label"] = labels[r["op"]]
    cols = ["request_to_agent", "pre_write", "writing", "post_write", "platform_tests", "after_agent", "ttfc", "ttvc_live", "total"]
    heads = ["req->agent", "pre-write", "writing", "post-write", "plat.test", "after", "TTFC", "TTVC-live", "total"]
    print("%-3s %-5s " % ("#", "id") + " ".join("%9s" % h for h in heads))
    for i, r in enumerate(sorted(rows, key=lambda r: r["t0"]), 1):
        print("%-3d %-5s " % (i, r["label"]) + " ".join("%9s" % ("-" if r.get(c) is None else "%.1f" % r[c]) for c in cols))
    print("\npooled over %d successful runs (median / p90 / min-max):" % len(rows))
    for c, h in zip(cols, heads):
        v = [r[c] for r in rows if r.get(c) is not None]
        if v:
            print("  %-10s median %6.1f   p90 %6.1f   range %5.1f - %5.1f   (n=%d)" % (h, statistics.median(v), rep.pctl(v, 0.9), min(v), max(v), len(v)))
    print("\nper request (range only; n per request is small):")
    for lab in sorted(set(labels.values())):
        v = [r["total"] for r in rows if r["label"] == lab]
        f = [r["ttfc"] for r in rows if r["label"] == lab and r.get("ttfc") is not None]
        if v:
            print("  %s total %5.1f - %5.1f   TTFC %s   (n=%d)" % (lab, min(v), max(v), "%.1f - %.1f" % (min(f), max(f)) if f else "-", len(v)))


def do_teardown(a):
    code, d = cp(a.ssh, "DELETE", "/api/v1/apps/%s?purge=true" % APP)
    print("delete bench:", code)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--ssh", required=True)
    g = ap.add_mutually_exclusive_group(required=True)
    g.add_argument("--setup", action="store_true"); g.add_argument("--run", action="store_true")
    g.add_argument("--teardown", action="store_true"); g.add_argument("--summarize")
    ap.add_argument("--plan", default="B1,B2,B3,B4,B5"); ap.add_argument("--reps", type=int, default=2)
    ap.add_argument("--max-runs", type=int, default=12); ap.add_argument("--pause", type=float, default=10)
    ap.add_argument("--out", default="docs/measurements/data/ml1-%s.json" % dt.date.today().isoformat())
    ap.add_argument("--label", default=""); ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args()
    if a.setup: do_setup(a)
    elif a.run: do_run(a)
    elif a.teardown: do_teardown(a)
    else:
        # Several files can be combined (e.g. a pilot and the main run): trials are merged, the newest
        # file's operations/events (which contain all earlier ones) are used.
        files = [json.load(open(f)) for f in a.summarize.split(",")]
        merged = dict(files[-1]); merged["trials"] = [t for f in files for t in f["trials"]]
        summarize(merged, load_report())


if __name__ == "__main__":
    main()
