#!/bin/sh
# T4: 小さい同期書き込み。usage: t4_fio.sh <dir> [runs=3] [runtime=60]
# 結果は 1 行 1 JSON(IOPS と fsync 込みの書き込みレイテンシ p50/p99、単位 us)。
d=$1; runs=${2:-3}; rt=${3:-60}
i=1
while [ $i -le $runs ]; do
  fio --name=sync4k --directory="$d" --rw=randwrite --bs=4k --size=128m --fsync=1 --iodepth=1 \
      --ioengine=sync --runtime="$rt" --time_based --output-format=json > /tmp/fio.json
  python3 - <<'PY'
import json
job = json.load(open("/tmp/fio.json"))["jobs"][0]
j = job["write"]
# write() 自体の遅延(clat)と、fsync の遅延(job["sync"])は別。fsync を含む実際の 1 回の書き込みは後者。
c = j["clat_ns"]["percentile"]; s = job.get("sync", {}).get("lat_ns", {}).get("percentile", {})
print(json.dumps({"iops": round(j["iops"], 1), "clat_p50_us": round(c["50.000000"] / 1000, 1),
                  "clat_p99_us": round(c["99.000000"] / 1000, 1),
                  "fsync_p99_us": round(s["99.000000"] / 1000, 1) if s else None}))
PY
  rm -f "$d"/sync4k.*
  i=$((i+1))
done
