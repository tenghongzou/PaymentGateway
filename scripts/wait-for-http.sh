#!/usr/bin/env bash
# Wait until every given URL returns HTTP 2xx.
# Usage: scripts/wait-for-http.sh [-t TIMEOUT_SECONDS] [-i INTERVAL_SECONDS] URL...
set -euo pipefail

TIMEOUT=120
INTERVAL=2
while getopts "t:i:" opt; do
  case "$opt" in
    t) TIMEOUT="$OPTARG" ;;
    i) INTERVAL="$OPTARG" ;;
    *) echo "usage: $0 [-t timeout] [-i interval] URL..." >&2; exit 2 ;;
  esac
done
shift $((OPTIND - 1))

if [ "$#" -eq 0 ]; then
  echo "usage: $0 [-t timeout] [-i interval] URL..." >&2
  exit 2
fi

deadline=$(( $(date +%s) + TIMEOUT ))
for url in "$@"; do
  printf '>> waiting for %s ' "$url"
  until curl -fsS -o /dev/null --max-time 3 "$url"; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo " TIMEOUT after ${TIMEOUT}s"
      exit 1
    fi
    printf '.'
    sleep "$INTERVAL"
  done
  echo " ok"
done
