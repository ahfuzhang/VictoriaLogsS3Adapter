# Shared settings and helpers for the victoria_logs_s3_backup end-to-end experiment.
# Source this file from the other scripts; every value can be overridden via the environment.

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$E2E_DIR/../../.." && pwd)"
WORK_DIR="${E2E_WORK_DIR:-$REPO_DIR/.e2e/backup}"
LOG_DIR="$WORK_DIR/logs"

# RustFS (local S3-compatible object storage).
RUSTFS_IMAGE="${RUSTFS_IMAGE:-rustfs/rustfs:latest}"
RUSTFS_CONTAINER="${RUSTFS_CONTAINER:-vl-e2e-rustfs}"
RUSTFS_S3_PORT="${RUSTFS_S3_PORT:-19000}"
RUSTFS_CONSOLE_PORT="${RUSTFS_CONSOLE_PORT:-19001}"
RUSTFS_DATA="$WORK_DIR/rustfs-data"
S3_ENDPOINT="http://127.0.0.1:${RUSTFS_S3_PORT}"
S3_REGION="${S3_REGION:-us-east-1}"
S3_BUCKET="${S3_BUCKET:-vl-backup-e2e}"
S3_ACCESS_KEY="${S3_ACCESS_KEY:-e2e-access-key}"
S3_SECRET_KEY="${S3_SECRET_KEY:-e2e-secret-key-0123456789}"

# VictoriaLogs: the source instance that is backed up and a separate instance for restore checks.
VL_IMAGE="${VL_IMAGE:-victoriametrics/victoria-logs:v1.50.0}"
VL_CPUSET="${VL_CPUSET:-4,5,6,7}"
VL_SRC_CONTAINER="${VL_SRC_CONTAINER:-vl-e2e-source}"
VL_SRC_PORT="${VL_SRC_PORT:-9428}"
VL_SRC_DATA="$WORK_DIR/vl-source-data"
VL_SRC_ADDR="http://127.0.0.1:${VL_SRC_PORT}"
VL_RESTORE_CONTAINER="${VL_RESTORE_CONTAINER:-vl-e2e-restore}"
VL_RESTORE_PORT="${VL_RESTORE_PORT:-9429}"
VL_RESTORE_DATA="$WORK_DIR/vl-restore-data"
VL_RESTORE_ADDR="http://127.0.0.1:${VL_RESTORE_PORT}"
# Instances of the large-partition step (09_large_partition.sh).
VL_LARGE_CONTAINER="${VL_LARGE_CONTAINER:-vl-e2e-large}"
VL_LARGE_PORT="${VL_LARGE_PORT:-9430}"
VL_LARGE_RESTORE_CONTAINER="${VL_LARGE_RESTORE_CONTAINER:-vl-e2e-large-restore}"
VL_LARGE_RESTORE_PORT="${VL_LARGE_RESTORE_PORT:-9431}"

# Backup tool.
BACKUP_BIN="${BACKUP_BIN:-$REPO_DIR/build/victoria_logs_s3_backup}"
# Environment variables referenced by the -s3.prefix template.
export namespace="${namespace:-e2e}"
export env="${env:-test}"
S3_PREFIX_TMPL='victoria_logs/{namespace}/{env}'
S3_PREFIX="victoria_logs/${namespace}/${env}"

# Test data.
LOGS_PER_DAY="${LOGS_PER_DAY:-300000}"
EXPECTED_JSON="$WORK_DIR/expected.json"

# Partition dates: two sealed days and today (UTC).
utc_day() { # utc_day <days ago>
  python3 -c 'import sys,datetime as d; print((d.datetime.now(d.timezone.utc)-d.timedelta(days=int(sys.argv[1]))).strftime("%Y%m%d"))' "$1"
}
DAY_2="$(utc_day 2)"
DAY_1="$(utc_day 1)"
DAY_0="$(utc_day 0)"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

# s3curl issues a SigV4-signed request against RustFS using curl only,
# so S3 checks do not depend on the code under test.
s3curl() {
  curl -sS --aws-sigv4 "aws:amz:${S3_REGION}:s3" --user "${S3_ACCESS_KEY}:${S3_SECRET_KEY}" "$@"
}

# s3_list <prefix> prints "<size>\t<key>" for every object under prefix.
s3_list() {
  s3curl "${S3_ENDPOINT}/${S3_BUCKET}?list-type=2&prefix=$1" |
    python3 -c '
import sys, xml.etree.ElementTree as ET
root = ET.fromstring(sys.stdin.read())
ns = {"s": root.tag.split("}")[0].strip("{")} if root.tag.startswith("{") else {}
p = "s:" if ns else ""
for c in root.findall(p + "Contents", ns):
    print(c.find(p + "Size", ns).text + "\t" + c.find(p + "Key", ns).text)
'
}

# backup_argv sets BACKUP_CMD to the backup tool command line with the S3 and VictoriaLogs
# settings of this experiment. Extra flags are appended, so later flags override earlier ones.
# To run it in the background, use: backup_argv ...; "${BACKUP_CMD[@]}" & pid=$!
# ($! is then the PID of the backup tool itself, since env execs it.)
backup_argv() {
  BACKUP_CMD=(env "auth.access_key_id=${S3_ACCESS_KEY}" "auth.secret_access_key=${S3_SECRET_KEY}"
    "$BACKUP_BIN"
    -s3.endpoint="$S3_ENDPOINT"
    -s3.region="$S3_REGION"
    -s3.bucket="$S3_BUCKET"
    -s3.prefix="$S3_PREFIX_TMPL"
    -victoria.logs.addr="$VL_SRC_ADDR"
    -victoria.logs.storage.dir="$VL_SRC_DATA/"
    "$@")
}

# run_backup runs the backup tool in the foreground (see backup_argv).
run_backup() {
  backup_argv "$@"
  "${BACKUP_CMD[@]}"
}

# wait_http <url> <timeout seconds>
wait_http() {
  local url="$1" timeout="${2:-60}" i
  for ((i = 0; i < timeout; i++)); do
    if curl -fsS -o /dev/null "$url" 2>/dev/null; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# json_field <file> <msg> <field>: prints field of the last JSON log line with the given msg.
json_field() {
  python3 -c '
import json, sys
path, msg, field = sys.argv[1:4]
val = None
for line in open(path):
    try:
        rec = json.loads(line)
    except ValueError:
        continue
    if rec.get("msg") == msg and field in rec:
        val = rec[field]
print("" if val is None else val)
' "$1" "$2" "$3"
}
