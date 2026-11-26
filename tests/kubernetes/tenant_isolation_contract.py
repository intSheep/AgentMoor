"""Verify tenant isolation after a real API restart using public HTTP only."""
from collections import Counter
import hashlib
import json
import re
import urllib.error
import urllib.parse


DIMENSIONS = {"tenant", "project", "runtime", "adapter"}
REQUIRED_KINDS = {"workspace", "session", "artifact"}
DIGEST = re.compile(r"[a-f0-9]{64}\Z")
REF_FIELDS = ("kind", "id", "version", "sha256", "size", "content_path")


def read_json(client, route):
    return json.loads(client.request("GET", route))


def require_404(client, route):
    try:
        client.request("GET", route)
    except urllib.error.HTTPError as error:
        try:
            assert error.code == 404, "tenant denial must be HTTP 404"
            assert error.headers is not None, "tenant denial has no HTTP header evidence"
            assert all(name.lower() != "x-agentmoor-revision" for name in error.headers), "tenant denial exposed a revision header"
        finally:
            error.close()
    else:
        raise AssertionError("foreign tenant read unexpectedly succeeded")


def fixed_ref(row, scope):
    assert row["scope"] == scope, "owner asset came from a different allocation"
    ref = {key: row[key] for key in REF_FIELDS}
    assert ref["kind"] in REQUIRED_KINDS | {"skill"}
    assert isinstance(ref["id"], str) and ref["id"]
    assert type(ref["version"]) is int and ref["version"] > 0
    assert type(ref["size"]) is int and ref["size"] >= 0
    assert isinstance(ref["sha256"], str) and DIGEST.fullmatch(ref["sha256"])
    address = urllib.parse.urlsplit(ref["content_path"])
    assert not address.scheme and not address.netloc and not address.fragment and address.path == "/v1/content"
    assert urllib.parse.parse_qs(address.query, strict_parsing=True) == {
        "kind": [ref["kind"]], "id": [ref["id"]], "version": [str(ref["version"])],
    }, "asset content path must identify its exact historical version"
    return ref


def check_bytes(client, ref):
    raw = client.download(ref)
    assert len(raw) == ref["size"], "owner fixed-version size changed"
    assert hashlib.sha256(raw).hexdigest() == ref["sha256"], "owner fixed-version checksum changed"


def verify(client, foreign, scope, restart_evidence, *, scenario=None):
    """Check a legitimate reader whose allocation differs only by tenant.

    The restart evidence comes from PersistentContract.restart_queued_turn;
    this helper never creates that evidence or accesses Kubernetes/storage.
    """
    assert set(scope) == DIMENSIONS and all(isinstance(value, str) and value for value in scope.values())
    assert scenario is None or isinstance(scenario, str) and scenario
    assert isinstance(restart_evidence, dict), "a real queued API restart is required"
    uids = restart_evidence["api_pod_uids"]
    assert isinstance(uids, list) and len(uids) == 2 and all(isinstance(uid, str) and uid for uid in uids)
    assert uids[0] != uids[1], "API restart must replace its Pod"
    assert restart_evidence["accepted_state"] == restart_evidence["restored_state"] == "queued"
    assert type(restart_evidence["same_uuid_replays"]) is int and restart_evidence["same_uuid_replays"] == 2

    route = "/v1/tasks/" + scope["runtime"]
    assert client.route == foreign.route == route, "both tenants must request the same runtime"
    assert client.endpoint == foreign.endpoint, "both tenants must use the same API origin"
    own_context = read_json(client, "/v1/context")
    assert own_context["scope"] == scope and own_context.get("read") is True
    other_context = read_json(foreign, "/v1/context")
    other_scope = other_context["scope"]
    assert set(other_scope) == DIMENSIONS and other_scope["tenant"] != scope["tenant"]
    assert isinstance(other_scope["tenant"], str) and other_scope["tenant"]
    assert all(other_scope[key] == scope[key] for key in DIMENSIONS - {"tenant"}), "foreign scope must differ only by tenant"
    assert other_context.get("read") is True, "invalid or non-reader credentials do not prove isolation"
    assert other_context.get("write") is False and not any(other_context.get(key, False) for key in ("submit", "execute", "collect"))

    # Prove the same owner routes exist before expecting a foreign 404.
    owner_task = read_json(client, route)
    owner_checkpoint = read_json(client, route + "/checkpoint")
    assert owner_task["id"] == scope["runtime"]
    assert owner_checkpoint["session_id"] and owner_checkpoint["workspace"]["kind"] == "workspace"
    assert read_json(foreign, "/v1/assets") == [], "foreign tenant asset listing is not empty"
    assert read_json(foreign, "/v1/heads") == [], "foreign tenant head listing is not empty"
    require_404(foreign, route)
    require_404(foreign, route + "/checkpoint")

    rows = read_json(client, "/v1/assets")
    assert isinstance(rows, list) and rows, "owner history must not be empty"
    refs = [fixed_ref(row, scope) for row in rows]
    identities = [(ref["kind"], ref["id"], ref["version"]) for ref in refs]
    assert len(set(identities)) == len(refs), "owner history contains duplicate version identities"
    kinds = Counter(ref["kind"] for ref in refs)
    assert REQUIRED_KINDS <= kinds.keys(), "owner history must include workspace, session and artifact"
    for ref in refs:
        check_bytes(client, ref)
        require_404(foreign, ref["content_path"])

    # Fixed-version metadata and bytes must survive all foreign read attempts.
    after = {tuple(row[key] for key in ("kind", "id", "version")): fixed_ref(row, scope)
             for row in read_json(client, "/v1/assets")}
    for identity, ref in zip(identities, refs):
        assert after[identity] == ref, "foreign reads changed owner revision metadata"
        check_bytes(client, ref)

    proof = {
        "scenario": scenario, "api_pod_uids": list(uids),
        "accepted_state": "queued", "restored_state": "queued", "same_uuid_replays": 2,
        "foreign_context_read": True, "foreign_read_only": True, "only_tenant_differs": True,
        "foreign_assets_empty": True, "foreign_heads_empty": True,
        "task_denial_status": 404, "checkpoint_denial_status": 404, "content_denial_status": 404,
        "revision_header_absent": True, "content_denials": len(refs),
        "asset_versions_checked": len(refs), "owner_rechecked_versions": len(refs),
        "asset_kinds": sorted(kinds), "kind_counts": dict(sorted(kinds.items())),
    }
    print("CROSS_TENANT_RESULT " + json.dumps(proof, sort_keys=True), flush=True)
    return proof
