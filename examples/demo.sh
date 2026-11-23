#!/bin/sh
# This demo runs entirely in its own temporary directory.
set -eu
cd "$(dirname "$0")/.."
bin="$PWD/bin/agentmoor"
demo=$(mktemp -d)
pid=
cleanup() {
  if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  rm -rf "$demo"
}
trap cleanup EXIT
"$bin" init --data "$demo/platform"
"$bin" serve --data "$demo/platform" --listen 127.0.0.1:0 >"$demo/listen" &
pid=$!
attempt=0
while ! grep -q '^listening ' "$demo/listen"; do
  kill -0 "$pid"
  attempt=$((attempt+1))
  if [ "$attempt" -gt 100 ]; then exit 1; fi
  sleep 0.1
done
address=$(awk '/^listening / {print $2;exit}' "$demo/listen")
"$bin" issue --data "$demo/platform" --tenant example --project demo --runtime primary --adapter claude-code --write >"$demo/credential"
export AGENTMOOR_TOKEN_FILE="$demo/credential"
cp -R testdata/claude-code "$demo/work"
mkdir -p "$demo/work/out" "$demo/sessions"
echo 'A published file.' >"$demo/work/out/result.txt"
session=921b456d-939b-43c3-bb69-ff2e9138ff0b
echo "{\"type\":\"user\",\"sessionId\":\"$session\",\"message\":{\"role\":\"user\",\"content\":\"Hello\"}}" >"$demo/sessions/$session.jsonl"
python3 - "$demo" "$address" <<'PY'
import json, pathlib, sys
root = pathlib.Path(sys.argv[1]).resolve()
config = dict(scope=dict(tenant="example", project="demo", runtime="primary", adapter="claude-code"),
              endpoint="http://" + sys.argv[2], workspace=str(root/"work"), sessions=str(root/"sessions"),
              artifacts="out", state_dir=str(root/"state"))
(root/"worker.json").write_text(json.dumps(config))
PY
cfg="$demo/worker.json"
"$bin" sync --config "$cfg"
"$bin" sync --config "$cfg"
"$bin" assets --config "$cfg" | python3 -c '
import json, sys
assets = json.load(sys.stdin)
assert sorted(asset["kind"] for asset in assets) == ["artifact", "session", "skill", "workspace"]
for asset in assets:
    assert asset["version"] == 1
    print("{kind} v{version}  {id}".format(**asset))
'
"$bin" restore --config "$cfg" --kind workspace --id root --to "$demo/restored"
cmp "$demo/work/out/result.txt" "$demo/restored/out/result.txt"
echo 'Workspace restored and verified.'
