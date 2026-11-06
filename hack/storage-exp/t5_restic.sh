#!/bin/sh
# T5: restic の保存先を S3(Garage)にして、バックアップ → 全消し → 復元 → 照合。テスト用データのみ。
# usage: t5_restic.sh <endpoint> <bucket>    環境変数: AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
set -eu
ep=$1; bucket=$2
export RESTIC_REPOSITORY="s3:$ep/$bucket/restic-$(date +%s)" RESTIC_PASSWORD=lab-only-test-password AWS_DEFAULT_REGION=garage
W=/tmp/t5; rm -rf $W; mkdir -p $W/data/sub
i=0; while [ $i -lt 60 ]; do head -c $((20000 + i * 1000)) /dev/urandom > $W/data/sub/f$i.bin; i=$((i+1)); done
echo "hello garage" > $W/data/hello.txt
(cd $W/data && find . -type f | sort | xargs sha256sum) > $W/manifest.txt
n=$(wc -l < $W/manifest.txt); sz=$(du -sk $W/data | cut -f1)
s=$(date +%s.%N); restic init >/dev/null; t_init=$(echo "$(date +%s.%N) - $s" | bc)
s=$(date +%s.%N); restic backup $W/data >/dev/null; t_backup=$(echo "$(date +%s.%N) - $s" | bc)
rm -rf $W/data
s=$(date +%s.%N); restic restore latest --target $W/restore >/dev/null; t_restore=$(echo "$(date +%s.%N) - $s" | bc)
(cd $W/restore$W/data && find . -type f | sort | xargs sha256sum) > $W/after.txt
if diff -q $W/manifest.txt $W/after.txt >/dev/null; then match=true; else match=false; fi
s=$(date +%s.%N); restic check >/dev/null 2>&1 && chk=ok || chk=ERROR; t_check=$(echo "$(date +%s.%N) - $s" | bc)
printf '{"files":%s,"kb":%s,"sha256_match":%s,"restic_check":"%s","sec":{"init":%.2f,"backup":%.2f,"restore":%.2f,"check":%.2f}}\n' $n $sz $match $chk $t_init $t_backup $t_restore $t_check
