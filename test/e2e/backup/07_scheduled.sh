#!/usr/bin/env bash
# Checks the scheduled mode (-backup.yesterday.at=HHMM): the tool waits for the next HHMM (UTC),
# backs up the previous UTC day, keeps running, and stops gracefully on SIGTERM.
# The prefix uses ${YYYYMMDD_HHMISS}, so every run is stored under its own time-stamped prefix.
set -euo pipefail
source "$(dirname "$0")/env.sh"

# Fire one or two minutes from now, leaving at least 20 seconds for startup.
at="$(python3 -c '
import datetime as d
now = d.datetime.now(d.timezone.utc)
t = (now + d.timedelta(minutes=1 if now.second < 40 else 2)).replace(second=0, microsecond=0)
print(t.strftime("%H%M"))
')"
out="$LOG_DIR/scheduled.log"
log "starting scheduled backup at $at UTC (expect partition $DAY_1)"
backup_argv -backup.yesterday.at="$at" -max.upload.bytes.per.second=100mb \
  -s3.prefix='victoria_logs/{namespace}/{env}/scheduled/${YYYYMMDD_HHMISS}'
"${BACKUP_CMD[@]}" >"$out" 2>&1 &
pid=$!

for ((i = 0; i < 200; i++)); do
  grep -q '"msg":"backup completed"' "$out" && break
  kill -0 "$pid" 2>/dev/null || { cat "$out" >&2; die "scheduled backup exited early"; }
  sleep 1
done
grep -q '"msg":"backup completed"' "$out" || { kill "$pid"; cat "$out" >&2; die "scheduled backup did not run"; }
sleep 2
kill -0 "$pid" 2>/dev/null || die "the tool exited after the scheduled backup; it must keep running"
log "scheduled backup completed; process still running, next run: $(json_field "$out" "next backup scheduled" at)"

kill -TERM "$pid"
set +e
wait "$pid"
rc=$?
set -e
grep -q '"msg":"scheduler stopped"' "$out" || die "no 'scheduler stopped' log after SIGTERM"
log "SIGTERM -> exit code $rc"
[[ "$rc" == 0 ]] || die "unexpected exit code $rc"

key="$(json_field "$out" "backup completed" key)"
date="$(json_field "$out" "backup completed" date)"
[[ "$date" == "$DAY_1" ]] || die "scheduled backup took $date, expected $DAY_1"
log "objects under victoria_logs/$namespace/$env/scheduled/:"
s3_list "victoria_logs/$namespace/$env/scheduled/" | tee "$LOG_DIR/scheduled_objects.tsv" >&2
grep -q "$key" "$LOG_DIR/scheduled_objects.tsv" || die "object $key not found"
