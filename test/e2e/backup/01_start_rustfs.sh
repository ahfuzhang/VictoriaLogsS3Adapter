#!/usr/bin/env bash
# Starts RustFS as a local S3-compatible object storage and creates the backup bucket.
set -euo pipefail
source "$(dirname "$0")/env.sh"

docker rm -f "$RUSTFS_CONTAINER" >/dev/null 2>&1 || true
mkdir -p "$RUSTFS_DATA" "$LOG_DIR"

log "starting $RUSTFS_IMAGE as $RUSTFS_CONTAINER (S3 :$RUSTFS_S3_PORT, console :$RUSTFS_CONSOLE_PORT)"
docker run -d --name "$RUSTFS_CONTAINER" \
  -p "${RUSTFS_S3_PORT}:9000" \
  -p "${RUSTFS_CONSOLE_PORT}:9001" \
  -e RUSTFS_ACCESS_KEY="$S3_ACCESS_KEY" \
  -e RUSTFS_SECRET_KEY="$S3_SECRET_KEY" \
  -e RUSTFS_CONSOLE_ENABLE=true \
  -v "$RUSTFS_DATA:/data" \
  "$RUSTFS_IMAGE" /data >/dev/null

log "waiting for the S3 API"
for ((i = 0; i < 60; i++)); do
  code="$(s3curl -o /dev/null -w '%{http_code}' "${S3_ENDPOINT}/" 2>/dev/null || true)"
  [[ "$code" == "200" ]] && break
  sleep 1
done
[[ "$code" == "200" ]] || die "RustFS S3 API is not ready (last status: $code)"

code="$(s3curl -o "$LOG_DIR/create_bucket.xml" -w '%{http_code}' -X PUT "${S3_ENDPOINT}/${S3_BUCKET}")"
case "$code" in
  200) log "bucket $S3_BUCKET created" ;;
  409) log "bucket $S3_BUCKET already exists" ;;
  *) cat "$LOG_DIR/create_bucket.xml" >&2; die "cannot create bucket $S3_BUCKET (status $code)" ;;
esac

version="$(docker exec "$RUSTFS_CONTAINER" rustfs --version 2>/dev/null | head -1 || true)"
log "RustFS is ready: ${version:-unknown version}; endpoint=$S3_ENDPOINT bucket=$S3_BUCKET"
