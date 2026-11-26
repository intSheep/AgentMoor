# INT-67 publication fault proxy fixture

This test-only package imports AgentMoor's actual `internal/asset` definitions
and fingerprint algorithm. It is part of the main Go module, so the normal
`make check` includes its tests. Unit tests alone do not establish Kubernetes,
PostgreSQL/S3, native CLI or Pod-loss acceptance; those need the cluster runner.
Run these commands from the repository root:

```sh
go test -race ./tests/kubernetes/faultproxy/...
go build -o /tmp/agentmoor-faultproxy ./tests/kubernetes/faultproxy/cmd/faultproxy
/tmp/agentmoor-faultproxy --listen :7443 \
  --upstream https://agentmoor:7401 --ca /certs/ca.pem \
  --cert /certs/proxy.crt --key /certs/proxy.key
```

The Worker-facing listener uses TLS and HTTP/1.1. The proxy verifies the upstream
certificate and hostname against `--ca`, and also uses HTTP/1.1 upstream. It does
not use environment HTTP proxies. `--cert` must cover the proxy Service
hostname, and the Worker must trust its issuer. These are two verified TLS
connections, not a new mutual client-certificate authentication requirement.

The separate administration listener is always plain HTTP on **127.0.0.1:7444**.
It has no external-bind option; its handler also rejects non-loopback clients.
The cluster runner can use `kubectl exec` to reach it. Worker requests do not
have access to this administration mux.

## Administration API

`POST /arm` accepts exactly one JSON object; unknown fields are rejected:

```json
{
  "case_id": "commit-after-1",
  "stage": "commit",
  "timing": "after",
  "limit": 1,
  "target": {
    "kind": "workspace",
    "id": "turns/9a8ded69-f941-43a4-95d4-24648187c15c/root"
  },
  "hold_first": true
}
```

- `stage`: `begin`, `manifest`, `part`, or `commit`.
- `timing`: `before` returns 503 without calling upstream. `after` completes and
  closes a real upstream 2xx response, then closes the downstream TCP connection
  without sending an acknowledgement. An upstream non-2xx is forwarded normally,
  does not consume an injection hit, and cannot be recorded as a successful
  lost acknowledgement.
- `limit`: 1–3 selected injections. Only the first is held when `hold_first` is
  true. Cancellation of that request clears the barrier immediately.
- `target`: only an exact workspace `turns/<canonical UUID>/root` key. A valid
  typed Begin establishes its operation via `asset.Fingerprint`. Subsequent
  requests must use that operation and the same immutable identities.
- A case ID and target turn key can each be used once per proxy process.
  Arming another case while the current case is blocked returns 409. At most
  32 cases are retained; a full ledger returns 429 instead of discarding proof.

Success returns `200 {"armed":true}`. `GET /status` returns the active case, or
`null` before the first arm. `GET /ledger` returns all retained cases. The case
includes `operation`, typed `change`, `manifest_digest`, `wire_sha256`,
`part_target`, `hits`, `blocked`, `consistent`, `error`, and `events`.

`POST /release` returns `200 {"released":true}` and releases the current first-hit
barrier; it is idempotent. The runner should wait for `blocked:true`, inspect the
public SaaS checkpoint directly, alter its owned non-artifact proof input, then
release. No proxy mutex is held while waiting, so monitoring GETs and admin
status remain usable. The proxy does not execute or count native commands.

Each event records stage, operation, safe publication path, timestamps, actual
upstream status, injection/cancellation outcome and bounded identity summaries.
`manifest_digest` uses `asset.TarParts.Digest` on the canonical uncompressed
manifest; `wire_sha256` hashes its actual transmitted gzip bytes. Part events
record actual body SHA-256, size and operation path. Request headers, tokens,
bodies and URL queries are never retained in the ledger or error logs. Expected
transport failures return generic messages rather than embedding URLs.

Normal successful Prepare responses are validated against the fixed manifest.
The first missing digest becomes the unique part fault target. Concurrent other
parts are forwarded normally. That selected digest is not changed if a later
Prepare correctly omits an already successful upload. Repeated manifests must
retain both canonical and wire identities, and every part must match its
manifest digest and size. Drift returns 409 and marks `consistent:false`.

## Bounds and scope

Begin/manifest wire JSON, decompressed manifest JSON, and publication metadata
responses are bounded at 16 MiB. Part request bodies are bounded at 4 MiB. The
manifest also passes AgentMoor's real format/part/inline limits. Each case has a
4096-event cap; overflow returns 429. Ordinary GET content and untargeted traffic
use `httputil.ReverseProxy` streaming and propagate request cancellation rather
than buffering asset archives.

The tests use genuine TLS upstream and downstream servers, real HTTP requests,
an upstream response deliberately paused halfway through its body, and real
downstream TCP closure. Cluster acceptance must separately cover deployment,
public-HTTP driver observations, history/GC and the production 900-second
Pod-loss combination; these unit tests do not claim that coverage.

The adjacent [`native_count.sh`](../native_count.sh) wraps the trusted Codex
executable. Only a first argument of `exec` increments
`/run/task/native-executions` and writes `{"native_exec_count":N}` to
`/dev/termination-log`; `--version` and other commands pass through without
counting. Both count files are outside workspace and Worker journal roots.
The script then `exec`s the fixed `/usr/local/bin/codex` with unchanged arguments.
It is a runtime executable wrapper, not an outer shell around the Worker, so
the Worker remains PID 1. The runner should verify the terminated Worker's
message before retiring its Pod, or read the owned counter while a Pod is held.
