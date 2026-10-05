#!/usr/bin/env python3
"""Real-agent walk-through of the release strategy on the home cluster, with timings.

  hack/release-live.py --ssh ubuntu-26 [--id relbench] [--teardown]

Creates ONE throw-away app with {"strategy":"release"} and a real LLM agent, then:
  create -> preview -> approve -> change -> preview -> approve
recording, for every step, the request->ready time and (from the agent trace) the time to the
first / last file written. Also checks the preview and production through the real ingress-nginx.
Costs real LLM money (about 30-60 JPY). Touches only the app named by --id.
"""
import argparse, importlib.util, json, os, statistics, sys, time, datetime as dt

here = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("bench", os.path.join(here, "latency-bench.py"))
bench = importlib.util.module_from_spec(spec); spec.loader.exec_module(bench)

PROMPT = bench.BASE_PROMPT
CHANGE = "文字をもっと大きくして"


def P(s):
    return dt.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--ssh", required=True); ap.add_argument("--id", default="relbench")
    ap.add_argument("--teardown", action="store_true")
    ap.add_argument("--out", default=os.path.join(here, "..", "docs", "measurements", "data", "release-live-%s.json" % dt.date.today().isoformat()))
    a = ap.parse_args()
    host, app = a.ssh, a.id
    if app in ("weather", "shopping", "bench", "app-2ef57e"):
        sys.exit("refusing to use an existing app id")
    if a.teardown:
        print(bench.cp(host, "DELETE", "/api/v1/apps/%s?purge=true" % app)); return

    def get():
        c, d = bench.cp(host, "GET", "/api/v1/apps/%s" % app)
        return d if c == 200 else {}

    def wait(pred, timeout=900, what=""):
        t0 = time.time()
        while time.time() - t0 < timeout:
            d = get(); op = d.get("operation") or {}
            if op.get("state") in ("SUCCEEDED", "FAILED") and pred(d):
                return d
            time.sleep(3)
        raise SystemExit("timed out: " + what)

    def curl_host(hostname, path="/healthz"):
        rc, out, err = bench.ssh(host, "kubectl -n ingress-nginx exec deploy/ingress-nginx-controller -- curl -s -m 8 -o /dev/null -w '%%{http_code}' -H 'Host: %s' http://127.0.0.1%s" % (hostname, path))
        return out.strip()

    def source_of(name):
        rc, out, _ = bench.ssh(host, "kubectl -n aap-apps get deploy %s -o jsonpath='{.spec.template.metadata.annotations.aap\\.dev/source}'" % name)
        return out.strip()

    log = {"app": app, "startedAt": dt.datetime.now(dt.timezone.utc).isoformat(), "steps": []}

    def step(name, t0, d, extra=None):
        ops = bench.cp(host, "GET", "/api/v1/apps/%s/operations" % app)[1]["operations"]
        op = ops[0]
        tr = json.loads(op["trace"]) if op.get("trace") else {}
        r = {"step": name, "op": op["id"], "kind": op["kind"], "state": op["state"], "wall_s": round(time.time() - t0, 1),
             "op_s": round(P(op["updatedAt"]) - P(op["createdAt"]), 1)}
        if tr:
            t_req = P(op["createdAt"]) * 1000
            ms = lambda k: round((tr[k] - t_req) / 1000, 1) if tr.get(k) else None
            r.update({"to_agent_start_s": ms("t_agent_start"), "ttfc_s": ms("t_first_write"), "last_write_s": ms("t_last_write"),
                      "agent_end_s": ms("t_agent_end"), "writes": tr.get("writes"), "model": tr.get("model")})
        r.update(extra or {})
        log["steps"].append(r)
        print("  %-26s %s" % (name, {k: v for k, v in r.items() if k not in ("step", "op")}))
        json.dump(log, open(a.out, "w"), ensure_ascii=False, indent=1)

    print("== 1. create (release strategy, real agent)")
    t0 = time.time()
    c, d = bench.cp(host, "POST", "/api/v1/apps", {"id": app, "name": "計測用(release)", "prompt": PROMPT, "strategy": "release"})
    if c != 202:
        sys.exit("create failed: %s %s" % (c, d))
    d = wait(lambda d: d.get("phase") == "PREVIEW" and d.get("draftState") == "PREVIEW_READY", what="first preview")
    step("create -> preview", t0, d, {"production_exists": bool(source_of("aap-gen-" + app)), "preview_http": curl_host(app + "-preview.kanke-aap-gen.com")})

    print("== 2. approve -> release 1")
    t0 = time.time()
    bench.cp(host, "POST", "/api/v1/apps/%s/approve" % app)
    d = wait(lambda d: d.get("phase") == "READY" and d.get("liveRelease") == 1, what="release 1")
    step("approve (release 1)", t0, d, {"production_source": source_of("aap-gen-" + app), "prod_http": curl_host(app + ".kanke-aap-gen.com")})

    print("== 3. change (real agent) -> preview; production must stay on releases/1")
    t0 = time.time()
    bench.cp(host, "POST", "/api/v1/apps/%s/changes" % app, {"prompt": CHANGE})
    d = wait(lambda d: d.get("draftState") == "PREVIEW_READY", what="preview of the change")
    step("change -> preview", t0, d, {"production_source_during_preview": source_of("aap-gen-" + app), "preview_http": curl_host(app + "-preview.kanke-aap-gen.com")})

    print("== 4. approve -> release 2")
    t0 = time.time()
    bench.cp(host, "POST", "/api/v1/apps/%s/approve" % app)
    d = wait(lambda d: d.get("liveRelease") == 2, what="release 2")
    step("approve (release 2)", t0, d, {"production_source": source_of("aap-gen-" + app), "prod_http": curl_host(app + ".kanke-aap-gen.com")})

    code, rel = bench.cp(host, "GET", "/api/v1/apps/%s/releases" % app)
    log["releases"] = rel
    print("== releases:", [(r["n"], r["sourceHash"][:12]) for r in rel["releases"]])
    log["finishedAt"] = dt.datetime.now(dt.timezone.utc).isoformat()
    json.dump(log, open(a.out, "w"), ensure_ascii=False, indent=1)
    print("raw ->", os.path.relpath(a.out))


if __name__ == "__main__":
    main()
