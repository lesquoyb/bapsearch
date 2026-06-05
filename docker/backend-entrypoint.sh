#!/bin/sh
# Start the long-lived trafilatura extraction service alongside the Go backend
# so page extraction doesn't spawn a Python process per URL. The backend POSTs
# HTML to it (BAP_TRAFILATURA_URL) and falls back to the CLI if it's down.
set -e

if [ "${BAP_TRAFILATURA_DISABLE_SERVICE:-}" != "true" ]; then
  # Keep the extractor up: restart it if it ever exits.
  (
    while true; do
      python3 /app/trafilatura_server.py || true
      echo "trafilatura service exited, restarting in 1s" >&2
      sleep 1
    done
  ) &

  # Wait (briefly) for it to finish importing trafilatura and start listening,
  # so the first searches hit a ready service instead of falling back to the CLI.
  port="${TRAFILATURA_PORT:-8090}"
  i=0
  while [ "$i" -lt 50 ]; do
    if curl -sf "http://127.0.0.1:${port}/health" >/dev/null 2>&1; then
      break
    fi
    i=$((i + 1))
    sleep 0.2
  done
fi

exec /app/bap-search-backend
