"""A pinned synthetic Skill that checks isolation from inside the real guest."""
import argparse
import hashlib
import io
import ipaddress
import json
from pathlib import Path
import re
import sys
import tarfile
import urllib.parse

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
from skill_report_contract import SkillCatalog

PROOF_PATH = ".isolation-proof.json"
PRIVATE_PATHS = (
    "/store/signing.key", "/data/signing.key", "/access/token", "/run/task/state",
    "/var/run/secrets/kubernetes.io/serviceaccount/token", "/model-auth/key",
    "/tls/key.pem", "/model-tls/key.pem", "/signing/signing.key",
)
SENTINEL = "/etc/agentmoor/readonly-sentinel"

# This is executable customer-side test code. It is only run from the restored
# Skill package inside the sandbox, never by the trusted Worker or this module.
PROBE_SOURCE = r'''import errno
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import socket
import stat
import urllib.parse
import uuid

PRIVATE_PATHS = (
    "/store/signing.key", "/data/signing.key", "/access/token", "/run/task/state",
    "/var/run/secrets/kubernetes.io/serviceaccount/token", "/model-auth/key",
    "/tls/key.pem", "/model-tls/key.pem", "/signing/signing.key",
)

def denied_file(path):
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NONBLOCK)
    except OSError as error:
        if error.errno in (errno.ENOENT, errno.EACCES, errno.EPERM):
            return True
        raise RuntimeError("private path probe had an unexpected failure") from None
    os.close(descriptor)
    raise RuntimeError("a forbidden private path could be opened")

def denied_tcp(address, required_port):
    value = urllib.parse.urlsplit("//" + address)
    host, port = value.hostname, value.port
    ip = ipaddress.ip_address(host)
    if ip.is_unspecified or port != required_port or value.path or value.query or value.fragment or value.username is not None or value.password is not None:
        raise RuntimeError("invalid public network canary")
    try:
        connection = socket.create_connection((host, port), timeout=1)
    except TimeoutError:
        return True
    except OSError:
        # Refused and no-route do not demonstrate an enforced packet DROP.
        raise RuntimeError("denied network canary did not time out") from None
    connection.close()
    raise RuntimeError("a forbidden control-plane port was reachable")

def main():
    rule = json.loads((Path(__file__).resolve().parents[1] / "references/rule.json").read_bytes())
    paths = {path: denied_file(path) for path in PRIVATE_PATHS}
    if any(name in ("AGENTMOOR_TOKEN", "AGENTMOOR_TOKEN_FILE") or name.startswith(("AWS", "PG", "KUBERNETES")) for name in os.environ):
        raise RuntimeError("a forbidden private environment variable was inherited")
    present = "OPENAI_API_KEY" in os.environ
    model_key = os.environ.get("OPENAI_API_KEY", "")
    only_capability = not present or re.fullmatch(r"sbx1\.[A-Za-z0-9_-]{43}", model_key) is not None
    if not only_capability:
        raise RuntimeError("model access did not use a delegated capability")
    sentinel = "/etc/agentmoor/readonly-sentinel"
    sentinel_mode = stat.S_IMODE(os.stat(sentinel).st_mode)
    if sentinel_mode != 0o666:
        raise RuntimeError("image sentinel permissions cannot prove a read-only mount")
    try:
        descriptor = os.open(sentinel, os.O_WRONLY | os.O_APPEND)
    except OSError as error:
        if error.errno != errno.EROFS:
            raise RuntimeError("image sentinel did not fail with EROFS") from None
    else:
        os.close(descriptor)
        raise RuntimeError("the image filesystem is writable")
    uid = os.getuid()
    if uid != 65532:
        raise RuntimeError("unexpected sandbox user")
    proof = {
        "version": "sandbox-isolation-proof.v1",
        "probe_nonce": uuid.uuid4().hex,
        "probe_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
        "private_paths_unreadable": paths,
        "forbidden_environment_absent": True,
        "model_env_present": present,
        "model_capability_only": only_capability,
        "model_capability_length": len(model_key) if present else 0,
        "api_canary": rule["api_canary"],
        "kubernetes_canary": rule["kubernetes_canary"],
        "api_tcp_denied_timeout": denied_tcp(rule["api_canary"], 7401),
        "kubernetes_tcp_denied_timeout": denied_tcp(rule["kubernetes_canary"], 443),
        "image_sentinel_mode": sentinel_mode,
        "image_readonly_erofs": True,
        "uid": uid,
    }
    encoded = (json.dumps(proof, sort_keys=True) + "\n").encode()
    if len(encoded) > 8192 or b"sbx1." in encoded:
        raise RuntimeError("isolation proof contains unexpected data")
    temporary = Path(".isolation-proof-" + uuid.uuid4().hex + ".tmp")
    try:
        with temporary.open("xb") as stream:
            stream.write(encoded)
        os.replace(temporary, ".isolation-proof.json")
    finally:
        temporary.unlink(missing_ok=True)

if __name__ == "__main__":
    main()
'''


def canary(address, required_port):
    value = urllib.parse.urlsplit("//" + address)
    ip = ipaddress.ip_address(value.hostname)
    if ip.is_unspecified or value.port != required_port or value.path or value.query or value.fragment or value.username is not None or value.password is not None:
        raise ValueError("fixed public IP/port canary required")
    return address


class IsolatedSkillCatalog(SkillCatalog):
    def __init__(self, endpoint, token, context, api_canary, kube_canary):
        self.api_canary = canary(api_canary, 7401)
        self.kube_canary = canary(kube_canary, 443)
        super().__init__(endpoint, token, context)

    def package(self, policy):
        _, members = SkillCatalog.package(policy)
        members["SKILL.md"] += b"The packaged scripts/probe.py also checks this sandbox's assigned execution boundary.\n"
        script = members["scripts/check.py"].decode()
        checked = "print('verified-skill-policy')\n"
        assert script.count(checked) == 1
        script = script.replace(checked,
                                "import subprocess\n"
                                "subprocess.run([sys.executable, '-I', str(Path(__file__).resolve().with_name('probe.py'))], check=True, timeout=8)\n" + checked)
        members["scripts/check.py"] = script.encode()
        members["scripts/probe.py"] = PROBE_SOURCE.encode()
        members["references/rule.json"] = (json.dumps({"policy": policy, "api_canary": self.api_canary,
                                                       "kubernetes_canary": self.kube_canary}, sort_keys=True) + "\n").encode()
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w") as archive:
            for name, raw in members.items():
                info = tarfile.TarInfo(name)
                info.mode, info.size = 0o600, len(raw)
                archive.addfile(info, io.BytesIO(raw))
        return stream.getvalue(), members

    def verify_proof(self, raw):
        return verify_proof(raw, self.api_canary, self.kube_canary)


def verify_proof(raw, api_canary=None, kube_canary=None):
    if isinstance(raw, dict):
        proof = raw
        encoded = json.dumps(raw, sort_keys=True).encode()
    else:
        encoded = raw.encode() if isinstance(raw, str) else raw
        assert isinstance(encoded, bytes) and len(encoded) <= 8192
        proof = json.loads(encoded)
    assert isinstance(proof, dict) and len(encoded) <= 8192 and b"sbx1." not in encoded
    required = {"version", "probe_nonce", "probe_sha256", "private_paths_unreadable", "forbidden_environment_absent",
                "model_env_present", "model_capability_only", "model_capability_length", "api_canary", "kubernetes_canary",
                "api_tcp_denied_timeout", "kubernetes_tcp_denied_timeout", "image_sentinel_mode", "image_readonly_erofs", "uid"}
    assert set(proof) == required and proof["version"] == "sandbox-isolation-proof.v1"
    assert isinstance(proof["probe_nonce"], str) and re.fullmatch(r"[a-f0-9]{32}", proof["probe_nonce"])
    assert proof["probe_sha256"] == hashlib.sha256(PROBE_SOURCE.encode()).hexdigest()
    paths = proof["private_paths_unreadable"]
    assert isinstance(paths, dict) and set(paths) == set(PRIVATE_PATHS) and all(value is True for value in paths.values())
    for name in ("forbidden_environment_absent", "model_capability_only", "api_tcp_denied_timeout", "kubernetes_tcp_denied_timeout", "image_readonly_erofs"):
        assert proof[name] is True
    assert type(proof["model_env_present"]) is bool and type(proof["model_capability_length"]) is int
    assert proof["model_capability_length"] == (48 if proof["model_env_present"] else 0)
    assert type(proof["image_sentinel_mode"]) is int and proof["image_sentinel_mode"] == 0o666
    assert type(proof["uid"]) is int and proof["uid"] == 65532
    canary(proof["api_canary"], 7401)
    canary(proof["kubernetes_canary"], 443)
    if api_canary is not None:
        assert proof["api_canary"] == api_canary
    if kube_canary is not None:
        assert proof["kubernetes_canary"] == kube_canary
    return proof


def check():
    # No isolation probe executes on the host. This checks packaging, source
    # syntax and strict acceptance/rejection of explicitly synthetic proofs.
    catalog = object.__new__(IsolatedSkillCatalog)
    catalog.api_canary, catalog.kube_canary = "10.96.0.8:7401", "10.96.0.1:443"
    packet, members = catalog.package("policy-offline-fixture")
    assert set(members) == {"SKILL.md", "scripts/check.py", "scripts/probe.py", "references/rule.json"}
    with tarfile.open(fileobj=io.BytesIO(packet)) as archive:
        assert all(entry.mode == 0o600 and archive.extractfile(entry).read() == members[entry.name] for entry in archive)
    assert b"subprocess.run" in members["scripts/check.py"] and b"timeout=8" in members["scripts/check.py"]
    for name in ("scripts/check.py", "scripts/probe.py"):
        compile(members[name], name, "exec")
    proof = {"version": "sandbox-isolation-proof.v1", "probe_nonce": "a" * 32,
             "probe_sha256": hashlib.sha256(PROBE_SOURCE.encode()).hexdigest(),
             "private_paths_unreadable": {path: True for path in PRIVATE_PATHS},
             "forbidden_environment_absent": True, "model_env_present": False, "model_capability_only": True,
             "model_capability_length": 0, "api_canary": catalog.api_canary, "kubernetes_canary": catalog.kube_canary,
             "api_tcp_denied_timeout": True, "kubernetes_tcp_denied_timeout": True,
             "image_sentinel_mode": 0o666, "image_readonly_erofs": True, "uid": 65532}
    assert catalog.verify_proof(json.dumps(proof).encode()) == proof
    present = {**proof, "model_env_present": True, "model_capability_length": 48}
    assert catalog.verify_proof(present) == present
    for change in ({"model_capability_only": False}, {"model_capability_length": 48}, {"api_tcp_denied_timeout": False},
                   {"image_readonly_erofs": False}, {"image_sentinel_mode": 0o644}, {"uid": 0}, {"unexpected": "value"},
                   {"private_paths_unreadable": {path: 1 for path in PRIVATE_PATHS}},
                   {"unexpected": "sbx1." + "A" * 43}):
        try:
            catalog.verify_proof({**proof, **change})
        except AssertionError:
            pass
        else:
            raise AssertionError("invalid offline proof was accepted")
    print("isolated Skill package/proof checks passed; no host probe executed", flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", required=True)
    parser.parse_args()
    check()
