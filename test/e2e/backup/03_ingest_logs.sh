#!/usr/bin/env bash
# Writes deterministic logs for the two previous UTC days and today into the source VictoriaLogs,
# then checks that the source instance holds exactly the generated logs (baseline for the restore check).
set -euo pipefail
source "$(dirname "$0")/env.sh"

mkdir -p "$LOG_DIR"
rm -f "$EXPECTED_JSON"
log "ingesting $LOGS_PER_DAY logs per day for $DAY_2,$DAY_1,$DAY_0 into $VL_SRC_ADDR"
python3 "$E2E_DIR/vldata.py" ingest --addr "$VL_SRC_ADDR" --days "$DAY_2,$DAY_1,$DAY_0" \
  --per-day "$LOGS_PER_DAY" --expected "$EXPECTED_JSON"

# No explicit force_flush here: the newest logs stay in memory (-inmemoryDataFlushInterval=30s),
# so the backup has to rely on the force_flush call of the backup tool to include them.
sleep 3
for d in "$DAY_2" "$DAY_1" "$DAY_0"; do
  python3 "$E2E_DIR/vldata.py" verify --addr "$VL_SRC_ADDR" --day "$d" --expected "$EXPECTED_JSON" \
    >"$LOG_DIR/source_verify_$d.json" || { cat "$LOG_DIR/source_verify_$d.json" >&2; die "source check failed for $d"; }
  log "source $d: $(python3 -c 'import json,sys; r=json.load(open(sys.argv[1])); print("count=%d sha256=%s ok=%s" % (r["got"]["count"], r["got"]["sha256"][:16], r["ok"]))' "$LOG_DIR/source_verify_$d.json")"
done
log "partitions on disk: $(ls "$VL_SRC_DATA/partitions" | tr '\n' ' ')"
