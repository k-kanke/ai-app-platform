#!/bin/bash
# Longhorn のボリュームを使っている間の常駐メモリ(instance-manager など)を 10 秒おきに記録する。
# usage: lh-mem-under-load.sh [storageclass=longhorn]  (fio を 60 秒回しながら、worker-01/02 と longhorn-system の Pod を観測)
set -euo pipefail
cd "$(dirname "$0")"
k() { ssh -o BatchMode=yes ubuntu-26 "kubectl -n storage-lab $(printf '%q ' "$@")"; }
sed -e "s/__NAME__/lhmem/g" -e "s/__SC__/${1:-longhorn}/g" -e "s/__NODE__/ubuntu-worker-02/g" -e "s/__ACCESS__/ReadWriteOnce/g" bench-pod.yaml | ssh -o BatchMode=yes ubuntu-26 kubectl apply -f - >/dev/null
k wait --for=condition=Ready pod/bench-lhmem --timeout=180s >/dev/null
k exec bench-lhmem -- sh -c 'apk add --no-cache fio 2>&1 | tail -1'
echo "## idle(ボリューム 1 つをマウント済み) $(date -u +%T)"
ssh -o BatchMode=yes ubuntu-26 'kubectl -n longhorn-system top pod --no-headers | sort -k3 -h -r | head -3; kubectl top nodes --no-headers | grep worker'
( k exec bench-lhmem -- fio --name=m --directory=/data --rw=randwrite --bs=4k --size=128m --fsync=1 --iodepth=1 --ioengine=sync --runtime=60 --time_based --output-format=terse > /dev/null ) &
for i in 1 2 3 4 5; do
  sleep 10; echo "## fio 実行中 +$((i*10))s $(date -u +%T)"
  ssh -o BatchMode=yes ubuntu-26 'kubectl -n longhorn-system top pod --no-headers | sort -k3 -h -r | head -3; kubectl top nodes --no-headers | grep worker'
done
wait
k delete pod/bench-lhmem pvc/bench-lhmem --wait=true
