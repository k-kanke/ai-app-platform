#!/usr/bin/env bash
# Storage benchmark for the Release / Preview design (docs/deep-dive-preview-release.md §3.7, §11).
#
# Question: what does "make an immutable Release of the working tree" and "clone production
# data for the Preview" cost, per storage backend, and which backends can ENFORCE a size limit?
#
# Backends compared (all on loop-device images, so it runs anywhere with a privileged Linux):
#   ext4-cp      ext4, full copy              <- what the worker does today (local-path on ext4/LVM)
#   xfs-cp       XFS, full copy (--reflink=never) <- same copy, different fs (control)
#   xfs-reflink  XFS, cp --reflink=always     <- copy-on-write file clone
#   lvm-thin     LVM thin snapshot            <- block-level copy-on-write snapshot
#
# Run (needs --privileged; the trees are mounted read-only):
#   docker run --rm --privileged \
#     -v "$PWD/hack/bench-storage.sh:/bench.sh:ro" \
#     -v "$PWD/portal/node_modules:/large:ro" -v "$PWD/<small tree>:/small:ro" \
#     alpine:3.20 sh -c 'apk add -q bash xfsprogs e2fsprogs util-linux lvm2 coreutils quota-tools xfsprogs-extra >/dev/null && bash /bench.sh'
set -uo pipefail
N=${N:-5}
W=${W:-/tmp/bench}
LARGE=${LARGE:-/large}
SMALL=${SMALL:-/small}
mkdir -p "$W"

now() { date +%s.%N; }
elapsed() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.3f", b-a}'; }
median() { sort -n | awk '{a[NR]=$1} END{ if(NR==0){print "n/a"} else printf "%.3f", (NR%2)?a[(NR+1)/2]:(a[NR/2]+a[NR/2+1])/2 }'; }
used_bytes() { df -B1 --output=used "$1" | tail -1 | tr -d ' '; }
mib() { awk -v b="$1" 'BEGIN{printf "%.1f", b/1048576}'; }
cleanup_mount() { umount "$1" 2>/dev/null; losetup -d "$2" 2>/dev/null; rm -f "$3"; }

row() { printf "| %-12s | %-6s | %12s | %14s | %12s |\n" "$@"; }

echo "kernel: $(uname -r) $(uname -m)   N=$N (median)"
for tree in small large; do
  src=$SMALL; [ "$tree" = large ] && src=$LARGE
  files=$(find "$src" -type f | wc -l); bytes=$(du -sb "$src" | cut -f1)
  echo
  echo "### tree=$tree  ($files files, $(mib "$bytes") MiB)"
  row backend tree "release (s)" "extra disk (MiB)" "rollback (s)"
  row ------------ ------ ------------ -------------- ------------

  for backend in ext4-cp xfs-cp xfs-reflink; do
    img=$W/$backend.img; M=$W/mnt-$backend; mkdir -p "$M"
    truncate -s 3G "$img"; L=$(losetup -f --show "$img")
    case $backend in ext4-cp) mkfs.ext4 -q -F "$L";; *) mkfs.xfs -q -f -m reflink=1 "$L";; esac
    mount "$L" "$M"; mkdir -p "$M/work"; cp -a "$src/." "$M/work/"; sync
    # GNU cp defaults to --reflink=auto (silently clones on XFS), so the "cp" rows must say never.
    cpflags="-a --reflink=never"; [ "$backend" = xfs-reflink ] && cpflags="-a --reflink=always"
    : > "$W/t.rel"; : > "$W/t.rb"; : > "$W/t.extra"
    for i in $(seq "$N"); do
      sync; u0=$(used_bytes "$M"); a=$(now)
      cp $cpflags "$M/work" "$M/rel$i"; sync
      b=$(now); u1=$(used_bytes "$M")
      elapsed "$a" "$b" >> "$W/t.rel"; echo $((u1-u0)) >> "$W/t.extra"
    done
    for i in $(seq "$N"); do   # rollback = replace the working tree with a release
      sync; a=$(now); rm -rf "$M/work"; cp $cpflags "$M/rel1" "$M/work"; sync; b=$(now)
      elapsed "$a" "$b" >> "$W/t.rb"
    done
    row "$backend" "$tree" "$(median < "$W/t.rel")" "$(mib "$(median < "$W/t.extra")")" "$(median < "$W/t.rb")"
    cleanup_mount "$M" "$L" "$img"
  done

  # --- LVM thin snapshot ---------------------------------------------------------------
  vg=benchvg
  if command -v lvm >/dev/null 2>&1; then
    export LVM_SUPPRESS_FD_WARNINGS=1
    mkdir -p /etc/lvm; cat > /etc/lvm/lvm.conf <<'EOF'
devices { global_filter = [ "a|/dev/loop.*|", "r|.*|" ]  scan = [ "/dev" ] }
activation { udev_sync = 0  udev_rules = 0  monitoring = 0 }
EOF
    img=$W/lvm.img; M=$W/mnt-lvm; mkdir -p "$M"; truncate -s 4G "$img"; L=$(losetup -f --show "$img")
    if pvcreate -q -ff -y "$L" >/dev/null 2>&1 && vgcreate -q "$vg" "$L" >/dev/null 2>&1 \
       && lvcreate -q -y --type thin-pool -L 3G -n pool "$vg" >/dev/null 2>&1 \
       && lvcreate -q -y -V 2G -T "$vg/pool" -n work >/dev/null 2>&1; then
      vgmknodes >/dev/null 2>&1
      mkfs.xfs -q -f "/dev/$vg/work" && mount "/dev/$vg/work" "$M" \
        && mkdir -p "$M/work" && cp -a "$src/." "$M/work/" && sync
      pool_used() { lvs --noheadings --units b --nosuffix -o data_percent,lv_size "$vg/pool" 2>/dev/null \
                    | awk '{printf "%.0f", $1/100*$2}'; }
      : > "$W/t.rel"; : > "$W/t.extra"
      for i in $(seq "$N"); do
        sync; u0=$(pool_used); a=$(now)
        lvcreate -q -s -n "rel$i" "$vg/work" >/dev/null 2>&1
        b=$(now); u1=$(pool_used)
        elapsed "$a" "$b" >> "$W/t.rel"; echo $((u1-u0)) >> "$W/t.extra"
      done
      # rollback = merge a snapshot back into the origin (needs the origin unmounted)
      : > "$W/t.rb"
      for i in 1 2 3; do
        sync; umount "$M"; vgmknodes >/dev/null 2>&1
        a=$(now)
        lvconvert -q -y --mergesnapshot "$vg/rel$i" >/dev/null 2>&1; vgmknodes >/dev/null 2>&1
        mount "/dev/$vg/work" "$M" 2>/dev/null; b=$(now)
        elapsed "$a" "$b" >> "$W/t.rb"
      done
      row "lvm-thin" "$tree" "$(median < "$W/t.rel")" "$(mib "$(median < "$W/t.extra")")" "$(median < "$W/t.rb")"
      umount "$M" 2>/dev/null; lvremove -q -f "$vg" >/dev/null 2>&1; vgremove -q -f "$vg" >/dev/null 2>&1
    else
      why=$(lvcreate -y --type thin-pool -L 1G -n probe "$vg" 2>&1 | grep -m1 -oE "Required device-mapper target.*|not detected.*" || true)
      row "lvm-thin" "$tree" "(unavailable)" "-" "-"
      [ -n "${why:-}" ] && echo "  note: this kernel has no dm-thin-pool target; run on a real Linux host" || true
    fi
    pvremove -q -ff -y "$L" >/dev/null 2>&1; losetup -d "$L" 2>/dev/null; rm -f "$img"
  else
    row "lvm-thin" "$tree" "(no lvm)" "-" "-"
  fi
done

# --- Quota: can the filesystem ENFORCE a size limit on one app's directory? ---------------
echo
echo "### quota: limit one directory to 10 MiB, then write 100 MiB into it"
q() { # name  result
  printf "| %-26s | %s |\n" "$1" "$2"; }
q "backend" "result of writing 100 MiB into a 10 MiB-limited directory"
q "--------------------------" "----------------------------------------------"

try_dd() {
  local out; out=$(dd if=/dev/zero of="$1/big" bs=1M count=100 2>&1); local rc=$?
  local wrote; wrote=$(du -m "$1/big" 2>/dev/null | cut -f1)
  if [ "$rc" -ne 0 ] && echo "$out" | grep -qiE "No space|quota"; then echo "ENFORCED: write failed after ${wrote} MiB"
  else echo "NOT enforced: wrote ${wrote} MiB"; fi
  rm -f "$1/big"; }

# 1) plain ext4 (today)
img=$W/q1.img; M=$W/q1; mkdir -p $M; truncate -s 1G $img; L=$(losetup -f --show $img); mkfs.ext4 -q -F $L; mount $L $M; mkdir $M/app
q "ext4, no quota (today)" "$(try_dd $M/app)"; cleanup_mount $M $L $img

# 2) ext4 project quota
img=$W/q2.img; M=$W/q2; mkdir -p $M; truncate -s 1G $img; L=$(losetup -f --show $img)
if mkfs.ext4 -q -F -O quota,project -E quotatype=prjquota $L 2>/dev/null && mount -o prjquota $L $M 2>/dev/null; then
  mkdir $M/app; chattr -p 42 +P $M/app 2>/dev/null; setquota -P 42 0 10240 0 0 $M 2>/dev/null
  q "ext4 + project quota" "$(try_dd $M/app)"
else q "ext4 + project quota" "(unavailable: this kernel has no quota support)"; fi; cleanup_mount $M $L $img

# 3) XFS project quota
img=$W/q3.img; M=$W/q3; mkdir -p $M; truncate -s 1G $img; L=$(losetup -f --show $img); mkfs.xfs -q -f $L
if mount -o prjquota $L $M 2>/dev/null; then
  mkdir $M/app; xfs_quota -x -c "project -s -p $M/app 42" $M >/dev/null 2>&1; xfs_quota -x -c "limit -p bhard=10m 42" $M >/dev/null 2>&1
  q "XFS + project quota" "$(try_dd $M/app)"
else q "XFS + project quota" "(not supported here)"; fi; cleanup_mount $M $L $img

# 4) a volume of its own (what an LVM / per-PVC block volume gives): the size IS the limit
img=$W/q4.img; M=$W/q4; mkdir -p $M; truncate -s 10M $img; L=$(losetup -f --show $img); mkfs.ext4 -q -F $L; mount $L $M; mkdir $M/app
q "separate 10 MiB volume" "$(try_dd $M/app)"; cleanup_mount $M $L $img

rm -rf "$W"
