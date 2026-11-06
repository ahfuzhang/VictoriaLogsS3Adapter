#!/usr/bin/env bash
# Large partition: the tar stream exceeds the 64 MiB multipart part size of the backup tool, so the
# S3 multipart upload path (several parts) is exercised end to end. A separate VictoriaLogs instance
# is used, so the data set of the other steps stays unchanged.
set -euo pipefail
source "$(dirname "$0")/env.sh"

LARGE_LOGS="${LARGE_LOGS:-2000000}"
PART_SIZE=$((64 * 1024 * 1024))
src_data="$WORK_DIR/vl-large-data"
src_addr="http://127.0.0.1:${VL_LARGE_PORT}"
dst_data="$WORK_DIR/vl-large-restore-data"
dst_addr="http://127.0.0.1:${VL_LARGE_RESTORE_PORT}"
expected="$WORK_DIR/expected_large.json"
d="$DAY_1"

docker rm -f "$VL_LARGE_CONTAINER" "$VL_LARGE_RESTORE_CONTAINER" >/dev/null 2>&1 || true
rm -rf "$src_data" "$dst_data" "$expected" "$WORK_DIR/download_large"
mkdir -p "$src_data" "$dst_data/partitions/$d" "$WORK_DIR/download_large" "$LOG_DIR"

log "starting $VL_LARGE_CONTAINER on :$VL_LARGE_PORT"
docker run -d --name "$VL_LARGE_CONTAINER" -p "${VL_LARGE_PORT}:9428" --cpuset-cpus="$VL_CPUSET" -m 2048m \
  -v "$src_data:/data/" -e GOMAXPROCS=4 "$VL_IMAGE" \
  -storageDataPath=/data/ -inmemoryDataFlushInterval=30s -memory.allowedPercent=80 -retentionPeriod=3d >/dev/null
wait_http "$src_addr/health" 60 || die "VictoriaLogs did not become healthy"

log "ingesting $LARGE_LOGS logs for $d"
python3 "$E2E_DIR/vldata.py" ingest --addr "$src_addr" --days "$d" --per-day "$LARGE_LOGS" --expected "$expected"
sleep 3
python3 "$E2E_DIR/vldata.py" verify --addr "$src_addr" --day "$d" --expected "$expected" >"$LOG_DIR/large_source_verify.json" ||
  die "source check failed"

out="$LOG_DIR/large_backup.log"
log "backing up $d"
run_backup -victoria.logs.addr="$src_addr" -victoria.logs.storage.dir="$src_data/" \
  -s3.prefix='victoria_logs/{namespace}/{env}/large' -backup.date="$d" >"$out" 2>&1 || { cat "$out" >&2; die "backup failed"; }
key="$(json_field "$out" "backup completed" key)"
tar_bytes="$(json_field "$out" "backup completed" bytes)"
files="$(json_field "$out" "backup completed" files)"
upload_s="$(json_field "$out" "backup completed" upload_duration_seconds)"
(( tar_bytes > PART_SIZE )) || die "tar has $tar_bytes bytes, not larger than one part ($PART_SIZE); increase LARGE_LOGS"
head="$(s3curl -I "${S3_ENDPOINT}/${S3_BUCKET}/${key}" | tr -d '\r')"
s3_size="$(awk -F': ' 'tolower($1)=="content-length"{print $2}' <<<"$head")"
etag="$(awk -F': ' 'tolower($1)=="etag"{print $2}' <<<"$head")"
[[ "$s3_size" == "$tar_bytes" ]] || die "object size $s3_size != reported $tar_bytes"
[[ "$(curl -fsS "$src_addr/internal/partition/snapshot/list")" == "[]" ]] || die "snapshot not cleaned up"
log "uploaded s3://$S3_BUCKET/$key: $tar_bytes bytes, $files files, ${upload_s}s, ETag $etag"

tarfile="$WORK_DIR/download_large/$d.tar"
s3curl -f -o "$tarfile" "${S3_ENDPOINT}/${S3_BUCKET}/${key}"
nfiles="$(tar -tvf "$tarfile" | grep -c '^-')"
[[ "$nfiles" == "$files" ]] || die "tar has $nfiles files, backup tool reported $files"
tar -xf "$tarfile" -C "$dst_data/partitions/$d" --strip-components=1

log "starting $VL_LARGE_RESTORE_CONTAINER on the restored data at :$VL_LARGE_RESTORE_PORT"
docker run -d --name "$VL_LARGE_RESTORE_CONTAINER" -p "${VL_LARGE_RESTORE_PORT}:9428" -m 2048m \
  -v "$dst_data:/data/" -e GOMAXPROCS=4 "$VL_IMAGE" -storageDataPath=/data/ -retentionPeriod=100y >/dev/null
wait_http "$dst_addr/health" 60 || die "restored VictoriaLogs did not become healthy"
python3 "$E2E_DIR/vldata.py" verify --addr "$dst_addr" --day "$d" --expected "$expected" >"$LOG_DIR/large_restore_verify.json" ||
  { cat "$LOG_DIR/large_restore_verify.json" >&2; die "restore check failed"; }

printf 'date\tlogs\ttar_bytes\tparts(etag)\tfiles\tupload_s\tsource_sha256\trestored_sha256\n%s\t%s\t%s\t%s\t%s\t%.2f\t%s\t%s\n' \
  "$d" "$LARGE_LOGS" "$tar_bytes" "$etag" "$files" "$upload_s" \
  "$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["got"]["sha256"][:16])' "$LOG_DIR/large_source_verify.json")" \
  "$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["got"]["sha256"][:16])' "$LOG_DIR/large_restore_verify.json")" \
  >"$LOG_DIR/large_partition.tsv"
column -t -s $'\t' "$LOG_DIR/large_partition.tsv" >&2
log "large partition backup restored and verified"
