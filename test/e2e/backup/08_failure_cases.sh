#!/usr/bin/env bash
# Failure scenarios: the tool must exit with a non-zero code and a clear error, never leave a
# (partial) object in S3 and never leak a VictoriaLogs snapshot.
set -euo pipefail
source "$(dirname "$0")/env.sh"

summary="$LOG_DIR/failure_cases.tsv"
printf 'case\texit\terror\tobject_created\tsnapshots_left\tresult\n' >"$summary"
fail=0

snapshots_left() {
  local api dirs
  api="$(curl -fsS "$VL_SRC_ADDR/internal/partition/snapshot/list")"
  dirs="$(find "$VL_SRC_DATA/partitions" -path '*/snapshots/*' -mindepth 3 -maxdepth 3 | wc -l | tr -d ' ')"
  [[ "$api" == "[]" && "$dirs" == 0 ]] && echo 0 || echo "api=$api dirs=$dirs"
}

object_exists() { # object_exists <key>
  [[ "$(s3curl -o /dev/null -w '%{http_code}' -I "${S3_ENDPOINT}/${S3_BUCKET}/$1")" == 200 ]] && echo yes || echo no
}

# record <name> <exit code> <log file> <expected error substring> <key that must not exist>
record() {
  local name="$1" rc="$2" out="$3" want="$4" key="$5" err obj left res=PASS
  err="$(python3 -c '
import json, sys
msg = ""
for line in open(sys.argv[1]):
    line = line.strip()
    try:
        r = json.loads(line)
    except ValueError:
        msg = msg or line
        continue
    if r.get("level") == "ERROR":
        msg = r.get("error") or r.get("msg")
print(msg[:160])
' "$out")"
  obj="$(object_exists "$key")"
  left="$(snapshots_left)"
  if [[ "$rc" == 0 || "$err" != *"$want"* || "$obj" != no || "$left" != 0 ]]; then
    res=FAIL
    fail=1
  fi
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$name" "$rc" "$err" "$obj" "$left" "$res" >>"$summary"
  log "$name: exit=$rc object=$obj snapshots_left=$left -> $res"
}

# run_case <name> <expected error> <key> -- <command...>
run_case() {
  local name="$1" want="$2" key="$3" out="$LOG_DIR/failure_$1.log" rc
  shift 4
  set +e
  "$@" >"$out" 2>&1
  rc=$?
  set -e
  record "$name" "$rc" "$out" "$want" "$key"
}

k() { echo "victoria_logs/$namespace/$env/failure/$1/$DAY_1.tar"; }
p() { echo "-s3.prefix=victoria_logs/{namespace}/{env}/failure/$1"; }

run_case missing_credentials "must be set" "$(k missing_credentials)" -- \
  env -u auth.access_key_id -u auth.secret_access_key "$BACKUP_BIN" -s3.endpoint="$S3_ENDPOINT" -s3.region="$S3_REGION" \
  -s3.bucket="$S3_BUCKET" "$(p missing_credentials)" -victoria.logs.addr="$VL_SRC_ADDR" \
  -victoria.logs.storage.dir="$VL_SRC_DATA/" -backup.date="$DAY_1"

run_case wrong_secret_key "cannot access S3 bucket" "$(k wrong_secret_key)" -- \
  env "auth.access_key_id=${S3_ACCESS_KEY}" "auth.secret_access_key=wrong-secret" "$BACKUP_BIN" \
  -s3.endpoint="$S3_ENDPOINT" -s3.region="$S3_REGION" -s3.bucket="$S3_BUCKET" "$(p wrong_secret_key)" \
  -victoria.logs.addr="$VL_SRC_ADDR" -victoria.logs.storage.dir="$VL_SRC_DATA/" -backup.date="$DAY_1"

run_case missing_bucket "does not exist" "$(k missing_bucket)" -- \
  run_backup -s3.bucket=no-such-bucket-e2e "$(p missing_bucket)" -backup.date="$DAY_1"

# If {namespace} were silently expanded to "", the object would land at victoria_logs/<env>/<date>.tar.
run_case env_var_unset 'environment variable "namespace"' "victoria_logs/$env/$DAY_1.tar" -- \
  env -u namespace bash -c "source '$E2E_DIR/env.sh' >/dev/null 2>&1; unset namespace; run_backup -backup.date=$DAY_1"

run_case vl_unreachable "is not reachable" "$(k vl_unreachable)" -- \
  run_backup -victoria.logs.addr=http://127.0.0.1:9 "$(p vl_unreachable)" -backup.date="$DAY_1"

run_case wrong_storage_dir "cannot access -victoria.logs.storage.dir" "$(k wrong_storage_dir)" -- \
  run_backup -victoria.logs.storage.dir=/nonexistent/vl "$(p wrong_storage_dir)" -backup.date="$DAY_1"

run_case partition_not_found "partition directory not found" "victoria_logs/$namespace/$env/failure/partition_not_found/20200101.tar" -- \
  run_backup "$(p partition_not_found)" -backup.date=20200101

run_case invalid_date "not a valid YYYYMMDD date" "$(k invalid_date)" -- \
  run_backup "$(p invalid_date)" -backup.date=20261332

run_case mutually_exclusive "mutually exclusive" "$(k mutually_exclusive)" -- \
  run_backup "$(p mutually_exclusive)" -backup.date="$DAY_1" -backup.yesterday.at=0015

# SIGTERM in the middle of a rate-limited upload: the multipart upload must not be completed
# and the snapshot must be deleted.
name=sigterm_during_upload
out="$LOG_DIR/failure_$name.log"
backup_argv "$(p $name)" -backup.date="$DAY_1" -max.upload.bytes.per.second=1mb
"${BACKUP_CMD[@]}" >"$out" 2>&1 &
pid=$!
for ((i = 0; i < 30; i++)); do grep -q '"msg":"uploading snapshot"' "$out" && break; sleep 0.5; done
sleep 4
kill -TERM "$pid"
set +e
wait "$pid"
rc=$?
set -e
record "$name" "$rc" "$out" "interrupted" "$(k $name)"

# RustFS goes down while the tar stream is being uploaded.
name=s3_down_during_upload
out="$LOG_DIR/failure_$name.log"
backup_argv "$(p $name)" -backup.date="$DAY_1" -max.upload.bytes.per.second=1mb
"${BACKUP_CMD[@]}" >"$out" 2>&1 &
pid=$!
for ((i = 0; i < 30; i++)); do grep -q '"msg":"uploading snapshot"' "$out" && break; sleep 0.5; done
sleep 4
docker stop "$RUSTFS_CONTAINER" >/dev/null
set +e
wait "$pid"
rc=$?
set -e
docker start "$RUSTFS_CONTAINER" >/dev/null
for ((i = 0; i < 60; i++)); do
  [[ "$(s3curl -o /dev/null -w '%{http_code}' "${S3_ENDPOINT}/" 2>/dev/null || true)" == 200 ]] && break
  sleep 1
done
record "$name" "$rc" "$out" "cannot upload" "$(k $name)"

column -t -s $'\t' "$summary" >&2
[[ "$fail" == 0 ]] || die "some failure cases did not behave as expected"
log "all failure cases behaved as expected"
