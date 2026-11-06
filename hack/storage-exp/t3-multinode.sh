#!/bin/bash
# T3(別ノード版): 同じ RWX の PVC を 2 つのノードの Pod で共有し、同じ SQLite(WAL)に同時に書く。
# usage: t3-multinode.sh <name> <storageclass> [procs-per-pod=2] [rows=500]
set -euo pipefail
cd "$(dirname "$0")"
name=$1; sc=$2; procs=${3:-2}; rows=${4:-500}
k() { ssh -o BatchMode=yes ubuntu-26 "kubectl -n storage-lab $(printf '%q ' "$@")"; }
sed -e "s/__NAME__/$name/g" -e "s/__SC__/$sc/g" -e "s/__NODE__/ubuntu-worker-02/g" -e "s/__ACCESS__/ReadWriteMany/g" bench-pod.yaml | ssh -o BatchMode=yes ubuntu-26 kubectl apply -f -
# 2 つ目の Pod(worker-01)。同じ PVC を使う。
ssh -o BatchMode=yes ubuntu-26 kubectl apply -f - <<YAML
apiVersion: v1
kind: Pod
metadata: { name: bench-$name-b, namespace: storage-lab }
spec:
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ubuntu-worker-01 }
  containers:
    - name: c
      image: alpine:3.22
      command: ["sleep","7200"]
      resources: { requests: { cpu: 100m, memory: 64Mi }, limits: { memory: 512Mi } }
      volumeMounts: [{ name: d, mountPath: /data }]
  volumes: [{ name: d, persistentVolumeClaim: { claimName: bench-$name } }]
YAML
k wait --for=condition=Ready pod/bench-$name pod/bench-$name-b --timeout=180s
for p in bench-$name bench-$name-b; do
  k exec $p -- sh -c 'apk add --no-cache python3 2>&1 | tail -1'
  k exec -i $p -- sh -c 'cat > /tmp/t3_sqlite.py' < t3_sqlite.py
done
echo "== init (worker-02)"; k exec bench-$name -- python3 /tmp/t3_sqlite.py /data $procs $rows init
echo "== 2 つの Pod から同時に書く (各 $procs プロセス × $rows 行)"
ra=$(mktemp); rb=$(mktemp)
( k exec bench-$name -- timeout 300 python3 /tmp/t3_sqlite.py /data $procs $rows join > $ra 2>&1 || echo '{"error":"timeout_or_crash"}' >> $ra ) &
( k exec bench-$name-b -- timeout 300 python3 /tmp/t3_sqlite.py /data $procs $rows join > $rb 2>&1 || echo '{"error":"timeout_or_crash"}' >> $rb ) &
wait
echo "-- pod A (worker-02)"; cut -c1-600 $ra; echo "-- pod B (worker-01)"; cut -c1-600 $rb
echo "== check (worker-02)"; k exec bench-$name -- python3 /tmp/t3_sqlite.py /data $procs $rows check || true
echo "expected_rows=$((2 * procs * rows))"
echo "== check (worker-01)"; k exec bench-$name-b -- python3 /tmp/t3_sqlite.py /data $procs $rows check || true
rm -f $ra $rb
k delete pod/bench-$name pod/bench-$name-b pvc/bench-$name --wait=true
