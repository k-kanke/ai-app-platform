#!/bin/bash
# T3 の診断: 条件を小さくして、NFS 上の SQLite が「遅い」のか「壊れる/固まる」のかを切り分ける。
# usage: diag-t3.sh <name> <storageclass> <node> [access]
set -euo pipefail
cd "$(dirname "$0")"
name=$1; sc=$2; node=$3; access=${4:-ReadWriteOnce}
k() { ssh -o BatchMode=yes ubuntu-26 "kubectl -n storage-lab $(printf '%q ' "$@")"; }
sed -e "s/__NAME__/$name/g" -e "s/__SC__/$sc/g" -e "s/__NODE__/$node/g" -e "s/__ACCESS__/$access/g" bench-pod.yaml | ssh -o BatchMode=yes ubuntu-26 kubectl apply -f -
k wait --for=condition=Ready pod/bench-$name --timeout=180s
k exec bench-$name -- sh -c 'apk add --no-cache python3 2>&1 | tail -1'
k exec -i bench-$name -- sh -c 'cat > /tmp/t3_sqlite.py' < t3_sqlite.py
for cfg in "1 200" "2 100" "4 100" "4 500"; do
  set -- $cfg
  echo "== procs=$1 rows=$2"
  k exec bench-$name -- sh -c 'pkill -f "[t]3_sqlite" || true' || true   # 別呼び出し(同じ sh -c に書くと自分に一致する)
  k exec bench-$name -- sh -c "timeout 180 python3 /tmp/t3_sqlite.py /data $1 $2 || echo '{\"error\":\"timeout_or_crash\"}'" | cut -c1-700
done
echo "== 残プロセス確認"; k exec bench-$name -- sh -c 'ps | grep -c "[t]3_sqlite"' || true
k delete pod/bench-$name pvc/bench-$name --wait=true
