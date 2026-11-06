#!/usr/bin/env python3
"""T4(S3 版): 4 KiB オブジェクトの PUT / GET を順に n 回。1 回ごとの時間(ms)の中央値と p99、毎秒の回数を JSON で出す。
usage: t4_s3.py <endpoint> <bucket> [n=300]   認証は環境変数 AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
"""
import json, os, sys, time, statistics
import boto3
from botocore.config import Config
ep, bucket = sys.argv[1], sys.argv[2]; n = int(sys.argv[3]) if len(sys.argv) > 3 else 300
s3 = boto3.client("s3", endpoint_url=ep, region_name="garage",
                  config=Config(s3={"addressing_style": "path"}, retries={"max_attempts": 1}))
body = os.urandom(4096); pre = f"t4-{int(time.time())}-"
def pct(a, p): a = sorted(a); return round(a[min(len(a) - 1, int(len(a) * p))], 2)
def run(fn):
    ts = []; err = 0; t0 = time.perf_counter()
    for i in range(n):
        t = time.perf_counter()
        try: fn(i)
        except Exception: err += 1
        ts.append((time.perf_counter() - t) * 1000)
    wall = time.perf_counter() - t0
    return {"n": n, "errors": err, "p50_ms": round(statistics.median(ts), 2), "p99_ms": pct(ts, 0.99), "ops_per_sec": round(n / wall, 1)}
put = run(lambda i: s3.put_object(Bucket=bucket, Key=f"{pre}{i}", Body=body))
get = run(lambda i: s3.get_object(Bucket=bucket, Key=f"{pre}{i}")["Body"].read())
for i in range(n):
    try: s3.delete_object(Bucket=bucket, Key=f"{pre}{i}")
    except Exception: pass
print(json.dumps({"put_4KiB": put, "get_4KiB": get}))
