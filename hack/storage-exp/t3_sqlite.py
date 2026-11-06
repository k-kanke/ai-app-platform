#!/usr/bin/env python3
"""T3: SQLite(WAL)を複数プロセスから書く。結果を JSON で標準出力へ。
usage: t3_sqlite.py <dir> [procs=4] [rows=10000]
各プロセスは自分の接続で、1 行ごとにコミット(synchronous=FULL)。locked は最大 50 回リトライし、回数を数える。
"""
import json, multiprocessing as mp, os, sqlite3, sys, time

d = sys.argv[1]; procs = int(sys.argv[2]) if len(sys.argv) > 2 else 4
rows = int(sys.argv[3]) if len(sys.argv) > 3 else 10000
db = os.path.join(d, "t3.db")

def worker(i, q):
    locked = other = failed = 0
    samples = set()
    c = sqlite3.connect(db, timeout=5, isolation_level=None)
    c.execute("PRAGMA synchronous=FULL")
    t0 = time.time()
    for n in range(rows):
        for attempt in range(50):
            try:
                c.execute("INSERT INTO t(p,n,v) VALUES (?,?,?)", (i, n, "x" * 100)); break
            except sqlite3.OperationalError as e:
                if "locked" in str(e) or "busy" in str(e): locked += 1
                else: other += 1; samples.add(str(e)[:80])
            except sqlite3.DatabaseError as e:
                other += 1; samples.add(str(e)[:80]); break
        else:
            failed += 1
    q.put({"proc": i, "locked": locked, "other_errors": other, "failed_rows": failed, "error_samples": sorted(samples)[:3], "sec": round(time.time() - t0, 2)})

def check():
    try:
        c = sqlite3.connect(db); integ = c.execute("PRAGMA integrity_check").fetchone()[0]
        count = c.execute("SELECT count(*) FROM t").fetchone()[0]
    except Exception as e:
        integ, count = f"ERROR: {e}", -1
    return integ, count

if __name__ == "__main__":
    # mode: full(既定。作成 → 書く → 確認) / init(作成のみ) / join(既存の DB に書く。別の Pod からの同時書き込み用) / check(確認のみ)
    mode = sys.argv[4] if len(sys.argv) > 4 else "full"
    if mode in ("full", "init"):
        for f in (db, db + "-wal", db + "-shm"):
            try: os.remove(f)
            except FileNotFoundError: pass
        c = sqlite3.connect(db); jm = c.execute("PRAGMA journal_mode=WAL").fetchone()[0]
        c.execute("CREATE TABLE t(id INTEGER PRIMARY KEY, p INT, n INT, v TEXT)"); c.commit(); c.close()
        if mode == "init":
            print(json.dumps({"mode": "init", "journal_mode": jm})); sys.exit(0)
    if mode == "check":
        integ, count = check(); print(json.dumps({"mode": "check", "integrity_check": integ, "actual": count})); sys.exit(0)
    if mode == "join":
        for _ in range(60):
            if os.path.exists(db): break
            time.sleep(0.5)
    q = mp.Queue(); ps = [mp.Process(target=worker, args=(i, q)) for i in range(procs)]
    t0 = time.time(); [p.start() for p in ps]; res = [q.get() for _ in ps]; [p.join() for p in ps]
    wall = round(time.time() - t0, 2)
    out = {"mode": mode, "procs": procs, "rows_each": rows, "expected": procs * rows,
           "locked_total": sum(r["locked"] for r in res), "other_errors_total": sum(r["other_errors"] for r in res),
           "failed_rows_total": sum(r["failed_rows"] for r in res),
           "error_samples": sorted({e for r in res for e in r["error_samples"]})[:5], "wall_sec": wall, "per_proc": res}
    if mode == "full":
        integ, count = check()
        out.update({"integrity_check": integ, "actual": count, "missing": procs * rows - count if count >= 0 else None})
    print(json.dumps(out))
