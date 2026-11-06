#!/usr/bin/env python3
"""T2 後の Garage の確認: n=1..N を HEAD で調べ、存在の区間と、LIST の結果を並べる。"""
import sys, boto3
from botocore.config import Config
N = int(sys.argv[1]) if len(sys.argv) > 1 else 830
s3 = boto3.client("s3", endpoint_url="http://garage.storage-lab.svc:3900", region_name="garage",
                  config=Config(s3={"addressing_style": "path"}, connect_timeout=3, read_timeout=8, retries={"max_attempts": 1}))
head = set()
for n in range(1, N + 1):
    try: s3.head_object(Bucket="lab-bench", Key=f"t2-s3/{n}"); head.add(n)
    except Exception: pass
lst = set()
for p in s3.get_paginator("list_objects_v2").paginate(Bucket="lab-bench", Prefix="t2-s3/"):
    lst |= {int(o["Key"].split("/")[1]) for o in p.get("Contents", [])}
def runs(s):
    s = sorted(s); out = []
    if not s: return out
    a = b = s[0]
    for k in s[1:]:
        if k != b + 1: out.append((a, b)); a = k
        b = k
    out.append((a, b)); return out
print("HEAD で存在:", len(head), runs(head))
print("LIST で存在:", len(lst), runs(lst))
