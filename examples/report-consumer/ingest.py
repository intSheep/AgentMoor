#!/usr/bin/env python3
"""Verify and record an AgentMoor report using only its public HTTP API."""

import argparse
import contextlib
import hashlib
import json
import os
import re
import sqlite3
import sys
import urllib.error
import urllib.parse
import urllib.request

MAX_BYTES = 512 * 1024
COMPONENT = re.compile(r"[A-Za-z0-9_.-]{1,128}")
CHECKSUM = re.compile(r"[a-f0-9]{64}")


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON field")
        result[key] = value
    return result


def load_json(raw):
    return json.loads(raw.decode("utf-8"), object_pairs_hook=unique_object)


def validate_report(value):
    if type(value) is not dict or set(value) != {"totals_cents"}:
        raise ValueError("report must contain only totals_cents")
    totals = value["totals_cents"]
    if type(totals) is not dict or not 1 <= len(totals) <= 100:
        raise ValueError("report must contain 1 to 100 regions")
    for name, cents in totals.items():
        if not COMPONENT.fullmatch(name) or type(cents) is not int or not -(2**63) <= cents < 2**63:
            raise ValueError("invalid region or integer cents")
    return value


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError("AgentMoor responses must not redirect")


def get(opener, endpoint, path, token):
    req = urllib.request.Request(endpoint + path, headers={"Authorization": "Bearer " + token})
    with opener.open(req, timeout=10) as response:
        body = response.read(MAX_BYTES + 1)
    if len(body) > MAX_BYTES:
        raise ValueError("AgentMoor response exceeds example size limit")
    return body


def fetch_report(endpoint, token, task_id, turn_id):
    endpoint = endpoint.rstrip("/")
    origin = urllib.parse.urlsplit(endpoint)
    if (
        not origin.hostname or origin.username is not None or origin.password is not None
        or origin.path or origin.query or origin.fragment
        or (origin.scheme != "https" and not (
            origin.scheme == "http" and origin.hostname in {"localhost", "127.0.0.1", "::1"}
        ))
    ):
        raise ValueError("use an HTTPS origin (HTTP is allowed only on loopback)")
    for value in (task_id, turn_id):
        if value is not None and (not COMPONENT.fullmatch(value) or value in {".", ".."}):
            raise ValueError("invalid task or turn identifier")
    route = "/v1/tasks/" + task_id
    if turn_id is not None:
        route += "/turns/" + turn_id
    opener = urllib.request.build_opener(NoRedirect())
    record = load_json(get(opener, endpoint, route, token))
    if record.get("state") != "succeeded" or record.get("id") != task_id or record.get("turn_id") != turn_id:
        raise ValueError("requested task or turn is not a matching successful record")
    artifacts = [ref for ref in record.get("assets", []) if ref.get("kind") == "artifact"]
    if len(artifacts) != 1:
        raise ValueError("successful record must have exactly one artifact")
    ref = artifacts[0]
    artifact, version, digest = ref["id"], ref["version"], ref["sha256"]
    if not isinstance(artifact, str) or not artifact or len(artifact) > 4096:
        raise ValueError("invalid artifact identity")
    if type(version) is not int or not 0 < version < 2**63 or not CHECKSUM.fullmatch(digest):
        raise ValueError("invalid artifact version or SHA-256")
    query = urllib.parse.urlencode({"kind": "artifact", "id": artifact, "version": version})
    pinned_path = "/v1/content?" + query
    url = urllib.parse.urlsplit(ref["content_path"])
    if (
        url.scheme or url.netloc or url.fragment or url.path != "/v1/content"
        or sorted(urllib.parse.parse_qsl(url.query, strict_parsing=True, max_num_fields=3))
        != sorted(urllib.parse.parse_qsl(query))
    ):
        raise ValueError("content path must match the exact versioned artifact")
    # Construct our own same-origin URL; do not send credentials to a receipt-supplied URL.
    raw = get(opener, endpoint, pinned_path, token)
    if hashlib.sha256(raw).hexdigest() != digest:
        raise ValueError("artifact SHA-256 mismatch")
    return endpoint, artifact, version, digest, validate_report(load_json(raw))


def store(db_path, identity, digest, report):
    payload = json.dumps(report, sort_keys=True, separators=(",", ":"))
    with contextlib.closing(sqlite3.connect(db_path, timeout=10)) as db, db:
        db.execute("""
            CREATE TABLE IF NOT EXISTS imported_reports (
                endpoint TEXT, scope TEXT, task_id TEXT, turn_id TEXT, artifact_id TEXT,
                version INTEGER, sha256 TEXT NOT NULL, report_json TEXT NOT NULL,
                PRIMARY KEY (endpoint, scope, task_id, turn_id, artifact_id, version)
            ) WITHOUT ROWID
        """)
        db.execute("BEGIN IMMEDIATE")
        existing = db.execute("""
            SELECT sha256, report_json FROM imported_reports
            WHERE (endpoint, scope, task_id, turn_id, artifact_id, version) = (?, ?, ?, ?, ?, ?)
        """, identity).fetchone()
        if existing is not None:
            if existing != (digest, payload):
                raise ValueError("a different report is already stored for this exact source version")
            return "already-imported"
        db.execute("INSERT INTO imported_reports VALUES (?, ?, ?, ?, ?, ?, ?, ?)", (*identity, digest, payload))
        return "imported"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--endpoint", required=True, help="AgentMoor HTTPS origin")
    parser.add_argument("--scope", required=True, help="Your application’s stable tenant/project binding for this token")
    parser.add_argument("--task", required=True)
    parser.add_argument("--turn")
    parser.add_argument("--expected", required=True, help="Independent expected totals JSON file, not the Agent’s output")
    parser.add_argument("--db", required=True)
    args = parser.parse_args()
    token = os.environ.get("AGENTMOOR_READ_TOKEN")
    if not token or not args.scope.strip():
        parser.error("set AGENTMOOR_READ_TOKEN and provide a nonempty --scope")
    try:
        with open(args.expected, "rb") as expected_file:
            raw_expected = expected_file.read(MAX_BYTES + 1)
        if len(raw_expected) > MAX_BYTES:
            raise ValueError("expected report exceeds example size limit")
        expected_report = validate_report(load_json(raw_expected))
        endpoint, artifact, version, digest, report = fetch_report(args.endpoint, token, args.task, args.turn)
        if report != expected_report:
            raise ValueError("report totals do not match the independent expected totals")
        identity = (endpoint, args.scope, args.task, args.turn or "", artifact, version)
        status = store(args.db, identity, digest, report)
    except (ValueError, TypeError, KeyError, AttributeError, RecursionError, OSError, sqlite3.Error) as exc:
        print(f"report not imported: {exc}", file=sys.stderr)
        return 1
    print(json.dumps({"status": status, "artifact_id": artifact, "version": version, "sha256": digest}))
    return 0


if __name__ == "__main__":
    sys.exit(main())
