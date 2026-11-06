#!/usr/bin/env bash
# Proves that the backups are usable without any code from this repository:
#   1. download every <date>.tar from RustFS with curl (SigV4);
#   2. inspect the archive with tar and compare it with the backup tool report;
#   3. unpack it into partitions/<date> of an empty data directory;
#   4. start a separate, fresh VictoriaLogs container on that directory;
#   5. compare the full content of every day with the ground truth and the source instance.
#
# Optional: RESTORE_DAYS="20261103 20261104" restores only these days; the other days must then be absent.
set -euo pipefail
source "$(dirname "$0")/env.sh"

restore_days="${RESTORE_DAYS:-$DAY_2 $DAY_1 $DAY_0}"
dl_dir="$WORK_DIR/download"
rm -rf "$dl_dir" "$VL_RESTORE_DATA"
mkdir -p "$dl_dir" "$VL_RESTORE_DATA/partitions" "$LOG_DIR"
docker rm -f "$VL_RESTORE_CONTAINER" >/dev/null 2>&1 || true

report="$LOG_DIR/restore_archives.tsv"
printf 'date\tsha256(tar)\tsize\troots\tfiles\tdirs\tfiles_in_report\n' >"$report"
for d in $restore_days; do
  key="$S3_PREFIX/$d.tar"
  tarfile="$dl_dir/$d.tar"
  code="$(s3curl -o "$tarfile" -w '%{http_code}' "${S3_ENDPOINT}/${S3_BUCKET}/${key}")"
  [[ "$code" == 200 ]] || die "cannot download s3://$S3_BUCKET/$key (status $code)"
  tar -tvf "$tarfile" >"$LOG_DIR/tar_list_$d.txt"
  roots="$(tar -tf "$tarfile" | cut -d/ -f1 | sort -u | tr '\n' ' ')"
  [[ "$(wc -w <<<"$roots")" -eq 1 ]] || die "$d.tar must have exactly one top-level directory, got: $roots"
  nfiles="$(grep -c '^-' "$LOG_DIR/tar_list_$d.txt" || true)"
  ndirs="$(grep -c '^d' "$LOG_DIR/tar_list_$d.txt" || true)"
  reported="$(json_field "$LOG_DIR/backup_$d.log" "backup completed" files)"
  [[ "$nfiles" == "$reported" ]] || die "$d.tar has $nfiles files, backup tool reported $reported"
  for sub in datadb/parts.json indexdb/parts.json; do
    tar -tf "$tarfile" | grep -qx "${roots% }/$sub" || die "$d.tar has no $sub"
  done
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$d" "$(shasum -a 256 "$tarfile" | cut -c1-16)" "$(stat -f %z "$tarfile")" \
    "$roots" "$nfiles" "$ndirs" "$reported" >>"$report"

  # The archive root is the snapshot name; it becomes partitions/<date> of the restored instance.
  mkdir -p "$VL_RESTORE_DATA/partitions/$d"
  tar -xf "$tarfile" -C "$VL_RESTORE_DATA/partitions/$d" --strip-components=1
  log "restored $key -> partitions/$d (root dir ${roots% }, $nfiles files)"
done
column -t -s $'\t' "$report" >&2

log "starting a fresh VictoriaLogs ($VL_RESTORE_CONTAINER) on the restored data at $VL_RESTORE_ADDR"
docker run -d --name "$VL_RESTORE_CONTAINER" \
  -p "${VL_RESTORE_PORT}:9428" \
  -m 2048m \
  -v "$VL_RESTORE_DATA:/data/" \
  -e GOMAXPROCS=4 \
  "$VL_IMAGE" \
  -storageDataPath=/data/ \
  -retentionPeriod=100y >/dev/null
wait_http "$VL_RESTORE_ADDR/health" 60 || die "restored VictoriaLogs did not become healthy"
curl -fsS "$VL_RESTORE_ADDR/internal/partition/list" >"$LOG_DIR/restore_partition_list.json"
log "partitions attached by the restored instance: $(cat "$LOG_DIR/restore_partition_list.json")"

fail=0
for d in "$DAY_2" "$DAY_1" "$DAY_0"; do
  out="$LOG_DIR/restore_verify_$d.json"
  if [[ " $restore_days " == *" $d "* ]]; then
    python3 "$E2E_DIR/vldata.py" verify --addr "$VL_RESTORE_ADDR" --day "$d" --expected "$EXPECTED_JSON" >"$out" || fail=1
    python3 "$E2E_DIR/vldata.py" verify --addr "$VL_SRC_ADDR" --day "$d" --expected "$EXPECTED_JSON" >"$LOG_DIR/source_verify_after_$d.json" || fail=1
  else
    python3 "$E2E_DIR/vldata.py" verify --addr "$VL_RESTORE_ADDR" --day "$d" --expected "$EXPECTED_JSON" --expect-empty >"$out" || fail=1
  fi
  log "restored $d: $(python3 -c '
import json, sys
r = json.load(open(sys.argv[1]))
if r.get("expect_empty"):
    print("expect empty, count=%d ok=%s" % (r["count"], r["ok"]))
else:
    print("count=%d sha256=%s ok=%s failed=%s" % (r["got"]["count"], r["got"]["sha256"][:16], r["ok"],
          [k for k, v in r["checks"].items() if not v]))
' "$out")"
done
[[ "$fail" == 0 ]] || die "restore verification failed; see $LOG_DIR/restore_verify_*.json"
log "restore verification passed"
