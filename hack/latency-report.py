#!/usr/bin/env python3
"""Latency report for the edit cycle (docs/deep-dive-inner-loop-latency.md).

Reads every operation and every stored phase event from the Control Plane, joins the Agent's
AAP_TRACE (if present), and prints a waterfall per operation plus median / p90 per kind.

  hack/latency-report.py --ssh ubuntu-26                  # fetch from the home cluster (read-only)
  hack/latency-report.py --from docs/measurements/data/x.json    # re-analyse saved raw data
  options: --save docs/measurements/data/<name>.json   write the raw data (so the report is reproducible)
           --label "free text"                          recorded in the saved data
           --since 2026-10-05T00:00:00Z                 only operations created after this time
           --exclude app-2ef57e,other                   leave out apps (e.g. made by the stub agent before traces existed)
           --md                                         print a Markdown block ready for the measurement log

Segments (seconds, all from timestamps; "-" = not available for that operation):
  request->agent  : request received -> the Agent's entrypoint started (setup Jobs, pod start)
  pre-write       : agent start -> FIRST file the Agent wrote           (trace)
  writing         : first write -> last write                            (trace)
  post-write      : last write -> agent exited (self-check, wrap-up)     (trace)
  platform tests  : npm install + contract checks + tests after the agent (trace)
  after agent     : Job finished -> READY (runtime start)
  TTFC            : request -> first visible change if Preview reflected writes live (needs trace)
  TTVC-live       : request -> last write + settle (3 s) (needs trace)
  total (TTP)     : request -> READY, what the family waits today
"""
import argparse, json, statistics, subprocess, sys, datetime as dt

SETTLE_S = 3.0  # how long without writes before we call the change "settled" (design assumption, §2A)


def P(s):
    return dt.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()




def fetch_via_ssh(host):
    """Fetch operations (with traces) and phase events through the portal pod. Read-only."""
    # Events are an SSE stream; read them with wget inside the pod (a hard time limit ends the stream).
    script = (
        "P=$(kubectl -n platform-system get pod -l app.kubernetes.io/name=portal -o name | head -1 | cut -d/ -f2);"
        "for a in $(kubectl -n platform-system exec $P -- node -e \"fetch('http://control-plane.platform-system.svc:8080/api/v1/apps')"
        ".then(r=>r.json()).then(j=>console.log(j.apps.map(a=>a.id).join(' ')))\"); do "
        "echo \"##APP $a\"; "
        "kubectl -n platform-system exec $P -- node -e \"fetch('http://control-plane.platform-system.svc:8080/api/v1/apps/$a/operations')"
        ".then(r=>r.json()).then(j=>console.log('##OPS '+JSON.stringify(j.operations)))\"; "
        "kubectl -n platform-system exec $P -- sh -c \"wget -q -T 3 -O - --header='Last-Event-ID: 0' "
        "http://control-plane.platform-system.svc:8080/api/v1/apps/$a/events 2>/dev/null | head -c 200000\"; "
        "done"
    )
    out = subprocess.run(["ssh", "-o", "BatchMode=yes", host, script], capture_output=True, text=True, timeout=300)
    if out.returncode != 0 and not out.stdout:
        sys.exit("fetch failed: " + out.stderr[-400:])
    apps, cur = {}, None
    for line in out.stdout.splitlines():
        if line.startswith("##APP "):
            cur = line[6:].strip(); apps[cur] = {"ops": [], "events": []}
        elif cur and line.startswith("##OPS "):
            apps[cur]["ops"] = json.loads(line[6:])
        elif cur and line.startswith("data: "):
            try:
                apps[cur]["events"].append(json.loads(line[6:]))
            except ValueError:
                pass
    return apps


def build_rows(apps, since=None, exclude=()):
    rows = []
    for app, d in apps.items():
        if app in exclude:
            continue
        ops = sorted(d["ops"], key=lambda o: o["createdAt"])
        for i, o in enumerate(ops):
            if o["kind"] not in ("create", "modify") or o["state"] != "SUCCEEDED":
                continue
            if since and P(o["createdAt"]) < P(since):
                continue
            evs = sorted([e for e in d["events"] if e["opId"] == o["id"]], key=lambda e: e["id"])
            t = {}
            for e in evs:
                if e["phase"] and e["phase"] not in t:
                    t[e["phase"]] = P(e["createdAt"])
            if "READY" not in t or any("取り消" in e["message"] for e in evs if e["phase"] == "READY"):
                continue
            t0 = P(o["createdAt"])
            row = {"app": app, "op": o["id"], "kind": o["kind"], "agent": None, "model": None,
                   "total": t["READY"] - t0, "t0": t0, "events": {k: v - t0 for k, v in t.items()}}
            tr = None
            if o.get("trace"):
                try:
                    tr = json.loads(o["trace"])
                except ValueError:
                    tr = None
            if tr:
                row["agent"], row["model"] = tr.get("agent"), tr.get("model")
                ms = lambda k: (tr[k] / 1000.0 - t0) if tr.get(k) else None  # absolute seconds relative to request
                a0, fw, lw, a1 = ms("t_agent_start"), ms("t_first_write"), ms("t_last_write"), ms("t_agent_end")
                c1, x1 = ms("t_checks_end"), ms("t_exit")
                row.update({
                    "request_to_agent": a0,
                    "pre_write": (fw - a0) if fw is not None and a0 is not None else None,
                    "writing": (lw - fw) if lw is not None and fw is not None else None,
                    "post_write": (a1 - lw) if a1 is not None and lw is not None else None,
                    "platform_tests": (x1 - a1) if x1 is not None and a1 is not None else None,
                    "after_agent": (t["READY"] - t0 - x1) if x1 is not None else None,
                    "ttfc": fw, "ttvc_live": (lw + SETTLE_S) if lw is not None else None,
                    "writes": tr.get("writes"),
                })
            else:
                # No trace (older operation): use the phase events only.
                gen, tst, stt = t.get("GENERATING"), t.get("TESTING"), t.get("STARTING")
                row.update({
                    "request_to_agent": (gen - t0) if gen else None,
                    "agent_work_events": ((tst or stt) - gen) if gen and (tst or stt) else None,
                    "platform_tests": (stt - tst) if tst and stt else None,
                    "after_agent": (t["READY"] - stt) if stt else None,
                })
            rows.append(row)
    return rows


def fmt(x, w=7):
    return ("%*s" % (w, "-")) if x is None else ("%*.1f" % (w, x))


def pctl(xs, q):
    xs = sorted(xs)
    if not xs:
        return None
    k = (len(xs) - 1) * q
    f, c = int(k), min(int(k) + 1, len(xs) - 1)
    return xs[f] + (xs[c] - xs[f]) * (k - f)


def report(rows, md=False):
    cols = ["request_to_agent", "pre_write", "writing", "post_write", "platform_tests", "after_agent", "ttfc", "ttvc_live", "total"]
    heads = ["req->agent", "pre-write", "writing", "post-write", "plat.test", "after", "TTFC", "TTVC-live", "total"]
    out = []
    out.append("| app | kind | n-traced | " + " | ".join(heads) + " |" if md else
               "%-12s %-7s " % ("app", "kind") + " ".join("%9s" % h for h in heads))
    if md:
        out.append("|---|---|---|" + "---|" * len(heads))
    for r in sorted(rows, key=lambda r: r["t0"]):
        vals = [r.get(c) for c in cols]
        if md:
            out.append("| %s | %s | %s | " % (r["app"], r["kind"], "yes" if r.get("pre_write") is not None else "no") +
                       " | ".join("-" if v is None else "%.1f" % v for v in vals) + " |")
        else:
            out.append("%-12s %-7s " % (r["app"], r["kind"]) + " ".join("%9s" % ("-" if v is None else "%.1f" % v) for v in vals))
    out.append("")
    for kind in ("modify", "create"):
        sub = [r for r in rows if r["kind"] == kind and r.get("agent") != "template"]
        if not sub:
            continue
        out.append("%s (n=%d, LLM agents only)" % (kind, len(sub)))
        for c, h in zip(cols, heads):
            v = [r[c] for r in sub if r.get(c) is not None]
            if v:
                out.append("  %-11s median %6.1f s   p90 %6.1f s   (n=%d)" % (h, statistics.median(v), pctl(v, 0.9), len(v)))
    return "\n".join(out)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--ssh"), ap.add_argument("--from", dest="src"), ap.add_argument("--save")
    ap.add_argument("--label", default=""), ap.add_argument("--since"), ap.add_argument("--md", action="store_true"), ap.add_argument("--exclude", default="")
    a = ap.parse_args()
    if a.src:
        data = json.load(open(a.src)); apps = data["apps"]
    elif a.ssh:
        apps = fetch_via_ssh(a.ssh)
        data = {"fetchedAt": dt.datetime.now(dt.timezone.utc).isoformat(), "label": a.label, "apps": apps}
        if a.save:
            json.dump(data, open(a.save, "w"), ensure_ascii=False, indent=1)
            print("saved raw data ->", a.save, file=sys.stderr)
    else:
        ap.error("give --ssh HOST or --from FILE")
    exclude = tuple(x for x in a.exclude.split(",") if x)
    print(report(build_rows(apps, a.since, exclude), md=a.md))


if __name__ == "__main__":
    main()
