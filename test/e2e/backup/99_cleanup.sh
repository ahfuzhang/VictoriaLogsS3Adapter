#!/usr/bin/env bash
# Removes the experiment containers and the work directory.
set -euo pipefail
source "$(dirname "$0")/env.sh"

for c in "$VL_LARGE_RESTORE_CONTAINER" "$VL_LARGE_CONTAINER" "$VL_RESTORE_CONTAINER" "$VL_SRC_CONTAINER" "$RUSTFS_CONTAINER"; do
  docker rm -f "$c" >/dev/null 2>&1 && log "removed container $c" || true
done
if [[ -d "$WORK_DIR" && "$WORK_DIR" == */* && "$WORK_DIR" != "/" && "$WORK_DIR" != "$HOME" ]]; then
  rm -rf "$WORK_DIR"
  log "removed $WORK_DIR"
fi
