#!/bin/sh
# Only the persistent contract exports selected-platform service images.
set -eu
if ! server_api=$(docker version --format '{{.Server.APIVersion}}'); then
    printf '%s\n' 'Persistent contract cannot read the Docker server API version; check Docker and DOCKER_API_VERSION.' >&2
    exit 1
fi
python3 - "$server_api" "${DOCKER_API_VERSION:-}" <<'PY'
import re
import sys

for name, value in (("server API", sys.argv[1]), ("DOCKER_API_VERSION", sys.argv[2])):
    if not value and name == "DOCKER_API_VERSION":
        continue
    if not re.fullmatch(r"[0-9]+\.[0-9]+", value) or tuple(map(int, value.split("."))) < (1, 48):
        print("Persistent contract requires Docker API 1.48+ for image save --platform; "
              + name + " is too old or invalid. Upgrade Docker or remove the old API override.", file=sys.stderr)
        sys.exit(1)
PY
help=$(docker image save --help)
case "$help" in
    *--platform*) ;;
    *)
        printf '%s\n' 'Persistent contract requires a Docker CLI with docker image save --platform (API 1.48+).' >&2
        exit 1
        ;;
esac
printf '%s\n' 'persistent preflight passed: Docker save --platform and API 1.48+ available.'
