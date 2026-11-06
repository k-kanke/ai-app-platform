#!/bin/bash
# Garage の試行(T4 S3 版・T5)。前提: storage-lab に Garage と Secret garage-lab-key がある。
# usage: run-garage.sh [node=ubuntu-worker-02]
set -euo pipefail
cd "$(dirname "$0")"
node=${1:-ubuntu-worker-02}; ep=http://garage.storage-lab.svc:3900
k() { ssh -o BatchMode=yes ubuntu-26 "kubectl -n storage-lab $(printf '%q ' "$@")"; }
ssh -o BatchMode=yes ubuntu-26 kubectl apply -f - <<YAML
apiVersion: v1
kind: Pod
metadata: { name: s3-bench, namespace: storage-lab }
spec:
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: $node }
  containers:
    - name: c
      image: alpine:3.22
      command: ["sleep","7200"]
      envFrom: [{ secretRef: { name: garage-lab-key } }]
      resources: { requests: { cpu: 100m, memory: 64Mi }, limits: { memory: 512Mi } }
YAML
k wait --for=condition=Ready pod/s3-bench --timeout=180s
k exec s3-bench -- sh -c 'apk add --no-cache py3-boto3 restic bc 2>&1 | tail -1; restic version'
k exec -i s3-bench -- sh -c 'cat > /tmp/t4_s3.py' < t4_s3.py
k exec -i s3-bench -- sh -c 'cat > /tmp/t5_restic.sh; chmod +x /tmp/t5_restic.sh' < t5_restic.sh
echo "== T4 S3 (n=3)"
for i in 1 2 3; do k exec s3-bench -- python3 /tmp/t4_s3.py $ep lab-bench 300; done
echo "== T5 restic -> Garage (n=3)"
for i in 1 2 3; do k exec s3-bench -- /tmp/t5_restic.sh $ep lab-restic; done
