#!/usr/bin/env bash
# Backs up the three partitions to RustFS with victoria_logs_s3_backup (one-shot mode, -backup.date)
# and checks the uploaded objects and the snapshot cleanup.
set -euo pipefail
source "$(dirname "$0")/env.sh"

[[ -x "$BACKUP_BIN" ]] || die "$BACKUP_BIN not found; run 'make build' first"
mkdir -p "$LOG_DIR"
summary="$LOG_DIR/backup_summary.tsv"
printf 'date\texit\tkey\tfiles\tdata_bytes\ttar_bytes\ts3_size\tupload_s\tsnapshots_left\n' >"$summary"

for d in "$DAY_2" "$DAY_1" "$DAY_0"; do
  out="$LOG_DIR/backup_$d.log"
  log "backing up partition $d (disk usage before: $(du -sk "$VL_SRC_DATA/partitions/$d" | cut -f1) KiB)"
  set +e
  run_backup -backup.date="$d" >"$out" 2>&1
  rc=$?
  set -e
  key="$(json_field "$out" "backup completed" key)"
  files="$(json_field "$out" "backup completed" files)"
  data_bytes="$(json_field "$out" "backup completed" data_bytes)"
  tar_bytes="$(json_field "$out" "backup completed" bytes)"
  upload_s="$(json_field "$out" "backup completed" upload_duration_seconds)"
  s3_size="$(s3curl -I "${S3_ENDPOINT}/${S3_BUCKET}/${key}" | tr -d '\r' | awk -F': ' 'tolower($1)=="content-length"{print $2}')"
  snaps="$(curl -fsS "$VL_SRC_ADDR/internal/partition/snapshot/list")"
  left="$(find "$VL_SRC_DATA/partitions/$d/snapshots" -mindepth 1 -maxdepth 1 2>/dev/null | wc -l | tr -d ' ')"
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$d" "$rc" "$key" "$files" "$data_bytes" "$tar_bytes" "$s3_size" "$upload_s" "api=$snaps dirs=$left" >>"$summary"
  [[ "$rc" == 0 ]] || { cat "$out" >&2; die "backup of $d failed"; }
  [[ "$s3_size" == "$tar_bytes" ]] || die "object size $s3_size != reported tar size $tar_bytes"
  [[ "$snaps" == "[]" && "$left" == 0 ]] || die "snapshot of $d was not cleaned up: api=$snaps dirs=$left"
  log "  ok: s3://$S3_BUCKET/$key size=$tar_bytes files=$files"
done

log "objects in s3://$S3_BUCKET/$S3_PREFIX/:"
s3_list "$S3_PREFIX/" | tee "$LOG_DIR/s3_objects.tsv" >&2
column -t -s $'\t' "$summary" >&2
