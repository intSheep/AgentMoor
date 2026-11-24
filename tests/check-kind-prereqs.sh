#!/bin/sh
# Exercise the preflight without invoking the host Docker or creating a cluster.
set -eu
cd "$(dirname "$0")/.."
state=$(mktemp -d)
trap 'rm -rf "$state"' 0
mkdir "$state/bin"
export LOG="$state/docker.log"
for tool in make go kind kubectl python3 openssl cc; do
    printf '#!/bin/sh\nexit 99\n' >"$state/bin/$tool"
    chmod +x "$state/bin/$tool"
done
cat >"$state/bin/docker" <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >>"$LOG"
[ "$*" = info ] || exit 99
exit "${DOCKER_STATUS:-0}"
STUB
chmod +x "$state/bin/docker"

preflight() {
    PATH="$state/bin" /bin/sh scripts/check-kind-prereqs.sh >"$state/out" 2>&1
}

unset CC
preflight
grep -F 'kind preflight passed' "$state/out"
[ "$(cat "$LOG")" = info ]

export CC='cc -O2'
preflight
export CC='nonexistent-compiler -O2'
if preflight; then exit 1; fi
grep -F 'Missing: C compiler configured by CC' "$state/out"
unset CC

export DOCKER_STATUS=1
if preflight; then exit 1; fi
grep -F 'Unavailable: Docker CLI is installed' "$state/out"

rm "$LOG" "$state/bin/kind" "$state/bin/kubectl"
if preflight; then exit 1; fi
grep -F 'Missing: kind' "$state/out"
grep -F 'Missing: kubectl' "$state/out"
[ ! -e "$LOG" ]

rm "$state/bin/docker" "$state/bin/cc"
if preflight; then exit 1; fi
grep -F 'Missing: docker' "$state/out"
grep -F 'Missing: C compiler' "$state/out"
printf '%s\n' 'kind preflight tests passed'
