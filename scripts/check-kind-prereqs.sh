#!/bin/sh
# Check the host before building binaries or creating a disposable cluster.
set -uf

missing=0
for tool in make go docker kind kubectl python3 openssl; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        printf 'Missing: %s (install it and add it to PATH).\n' "$tool" >&2
        missing=1
    fi
done

if [ -n "${CC:-}" ]; then
    # CC may include compiler arguments. Do not execute or evaluate it.
    # shellcheck disable=SC2086
    set -- $CC
    if [ "$#" -eq 0 ] || ! command -v "$1" >/dev/null 2>&1; then
        printf 'Missing: C compiler configured by CC (check CC and PATH).\n' >&2
        missing=1
    fi
elif ! command -v cc >/dev/null 2>&1 &&
     ! command -v gcc >/dev/null 2>&1 &&
     ! command -v clang >/dev/null 2>&1; then
    printf 'Missing: C compiler (install GCC or Clang, or set CC).\n' >&2
    missing=1
fi

if [ "$missing" -ne 0 ]; then
    exit 1
fi

if ! docker info >/dev/null 2>&1; then
    printf '%s\n' 'Unavailable: Docker CLI is installed, but the daemon is not reachable with the current context and permissions.' \
        'Start Docker and check your context/permissions; run docker info to see the underlying error.' >&2
    exit 1
fi

printf '%s\n' 'kind preflight passed: required commands found and Docker is reachable.'
