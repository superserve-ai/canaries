#!/bin/sh
set -eu

expected="${CANARY_DISK_TOKEN:?missing CANARY_DISK_TOKEN}"
actual="$(cat /tmp/canary-token)"

if [ "$actual" != "$expected" ]; then
  printf '%s\n' "disk token mismatch" >&2
  exit 1
fi

printf '%s\n' "verified"
