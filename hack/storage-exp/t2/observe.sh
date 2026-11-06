#!/bin/bash
# T2 の観測: 10 秒おきに、ノードと試験 Pod の状態をタイムスタンプ付きで記録する。
# usage: observe.sh <seconds> > timeline.txt
end=$(( $(date +%s) + $1 ))
while [ "$(date +%s)" -lt "$end" ]; do
  echo "### $(date -u +%H:%M:%S)"
  ssh -o BatchMode=yes -o ConnectTimeout=5 ubuntu-26 'kubectl get nodes --no-headers | awk "{print \"node\", \$1, \$2}"; kubectl -n storage-lab get pods -l t2=1 -o wide --no-headers | awk "{print \"pod\", \$1, \$2, \$3, \$7}"; kubectl -n longhorn-system get volumes.longhorn.io --no-headers -o custom-columns=N:.metadata.name,S:.status.state,R:.status.robustness 2>/dev/null | awk "{print \"lhvol\", \$0}"' 2>&1
  sleep 10
done
