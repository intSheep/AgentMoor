#!/bin/sh
# Test-only native executable wrapper. The Worker itself remains PID 1.
set -eu

if [ "${1-}" = exec ]; then
  counter=/run/task/native-executions
  count=0
  if [ -e "$counter" ]; then
    IFS= read -r count < "$counter"
  fi
  case "$count" in
    ''|*[!0-9]*)
      printf '%s\n' 'invalid native execution counter' >&2
      exit 1
      ;;
  esac
  count=$((count + 1))
  printf '%s\n' "$count" > "$counter"
  printf '{"native_exec_count":%s}\n' "$count" > /dev/termination-log
fi

exec /usr/local/bin/codex "$@"
