#!/usr/bin/env bash
# Checks -max.upload.bytes.per.second: backs up the same sealed partition with several limits
# and compares the measured upload throughput with the limit.
set -euo pipefail
source "$(dirname "$0")/env.sh"

limits="${RATE_LIMITS:-1mb 4mb 100mb}"
summary="$LOG_DIR/rate_limit.tsv"
printf 'limit\tlimit_bytes_per_s\ttar_bytes\tupload_s\tmeasured_bytes_per_s\tmeasured/limit\n' >"$summary"
for lim in $limits; do
  out="$LOG_DIR/rate_limit_$lim.log"
  log "backing up $DAY_1 with -max.upload.bytes.per.second=$lim"
  run_backup -backup.date="$DAY_1" -max.upload.bytes.per.second="$lim" \
    -s3.prefix="victoria_logs/{namespace}/{env}/ratelimit_$lim" >"$out" 2>&1 || { cat "$out" >&2; die "backup failed"; }
  limit_bps="$(json_field "$out" "uploading snapshot" max_upload_bytes_per_second)"
  tar_bytes="$(json_field "$out" "backup completed" bytes)"
  upload_s="$(json_field "$out" "backup completed" upload_duration_seconds)"
  measured="$(json_field "$out" "backup completed" throughput_bytes_per_second)"
  ratio="$(python3 -c 'import sys; print("%.3f" % (float(sys.argv[1]) / float(sys.argv[2])))' "$measured" "$limit_bps")"
  printf '%s\t%s\t%s\t%.2f\t%s\t%s\n' "$lim" "$limit_bps" "$tar_bytes" "$upload_s" "$measured" "$ratio" >>"$summary"
done
column -t -s $'\t' "$summary" >&2
