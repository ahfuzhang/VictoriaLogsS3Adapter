#!/usr/bin/env bash
# Starts the source VictoriaLogs instance whose partitions are backed up.
#
# Mirrors the production command line. Differences:
#   * -p port mapping instead of --network=host (Docker Desktop on macOS has no host networking by default);
#   * the data directory is a bind mount under $WORK_DIR, so the backup tool on the host can read snapshots.
set -euo pipefail
source "$(dirname "$0")/env.sh"

docker rm -f "$VL_SRC_CONTAINER" >/dev/null 2>&1 || true
mkdir -p "$VL_SRC_DATA"

log "starting $VL_IMAGE as $VL_SRC_CONTAINER on :$VL_SRC_PORT, data=$VL_SRC_DATA"
docker run -d --name "$VL_SRC_CONTAINER" \
  -p "${VL_SRC_PORT}:9428" \
  --cpuset-cpus="$VL_CPUSET" \
  -m 2048m \
  -v "$VL_SRC_DATA:/data/" \
  -e GOMAXPROCS=4 \
  "$VL_IMAGE" \
  -storageDataPath=/data/ \
  -inmemoryDataFlushInterval=30s \
  -memory.allowedPercent=80 \
  -retentionPeriod=3d >/dev/null

wait_http "$VL_SRC_ADDR/health" 60 || die "VictoriaLogs did not become healthy"
log "VictoriaLogs is ready at $VL_SRC_ADDR"
