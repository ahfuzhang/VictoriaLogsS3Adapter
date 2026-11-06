#!/usr/bin/env bash
# Runs the whole victoria_logs_s3_backup experiment from scratch. See REPORT.md.
# Containers are left running afterwards for manual inspection; remove them with ./99_cleanup.sh.
set -euo pipefail
cd "$(dirname "$0")"
source ./env.sh

(cd "$REPO_DIR" && make build)
./99_cleanup.sh
./01_start_rustfs.sh
./02_start_victorialogs.sh
./03_ingest_logs.sh
./04_backup.sh
RESTORE_DAYS="$DAY_1" ./05_restore_verify.sh # one partition alone is a complete, self-contained backup
./05_restore_verify.sh                       # all partitions
./06_rate_limit.sh
./07_scheduled.sh
./08_failure_cases.sh
./09_large_partition.sh
log "all experiment steps passed; logs: $LOG_DIR"
