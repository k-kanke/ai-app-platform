#!/bin/bash
# usage: run-bench.sh <name> <storageclass> <node> [accessMode=ReadWriteOnce] [keep]
# T4(fio n=3)と T3(SQLite n=3)を、storage-lab の Pod で実行して JSON に保存する。
# 前提: ssh ubuntu-26 で kubectl が使える。Pod/PVC は最後に消す(keep を付けると残す)。
set -euo pipefail
cd "$(dirname "$0")"
name=$1; sc=$2; node=$3; access=${4:-ReadWriteOnce}; keep=${5:-}
# ssh はコマンドを連結してリモートのシェルで再解釈するので、引数を %q でクォートして渡す。
k() { ssh -o BatchMode=yes ubuntu-26 "kubectl -n storage-lab $(printf '%q ' "$@")"; }
out="../../docs/measurements/data/storage-$name-$(date +%F).json"
sed -e "s/__NAME__/$name/g" -e "s/__SC__/$sc/g" -e "s/__NODE__/$node/g" -e "s/__ACCESS__/$access/g" bench-pod.yaml \
  | ssh -o BatchMode=yes ubuntu-26 kubectl apply -f -
k wait --for=condition=Ready pod/bench-$name --timeout=180s
for try in 1 2 3; do
  k exec bench-$name -- sh -c 'apk add --no-cache fio python3 2>&1 | tail -2' && break
  echo "apk add 失敗($try 回目)。10 秒後に再試行"; sleep 10
done
k exec bench-$name -- sh -c 'fio --version; python3 --version'
k exec -i bench-$name -- sh -c 'cat > /tmp/t3_sqlite.py' < t3_sqlite.py
k exec -i bench-$name -- sh -c 'cat > /tmp/t4_fio.sh; chmod +x /tmp/t4_fio.sh' < t4_fio.sh
echo "== T4 fio (n=3)"; t4=$(k exec bench-$name -- /tmp/t4_fio.sh /data 3 60); echo "$t4"
echo "== T3 sqlite (n=3)"; t3=""
for i in 1 2 3; do
  r=$(k exec bench-$name -- timeout 600 python3 /tmp/t3_sqlite.py /data 4 10000 || echo '{"error":"timeout_or_crash"}'); echo "$r" | cut -c1-220
  t3="$t3$r"$'\n'
done
T4="$t4" T3="$t3" python3 -I - "$out" "$name" "$sc" "$node" <<'PY'
import json, os, sys, datetime
out, name, sc, node = sys.argv[1:5]
t4 = [json.loads(l) for l in os.environ["T4"].strip().splitlines()]
t3 = [json.loads(l) for l in os.environ["T3"].strip().splitlines()]
json.dump({"name": name, "storageclass": sc, "node": node,
           "date": datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds"),
           "t4_fio": t4, "t3_sqlite": t3}, open(out, "w"), ensure_ascii=False, indent=1)
print("saved", out)
PY
[ "$keep" = keep ] || k delete pod/bench-$name pvc/bench-$name --wait=true
