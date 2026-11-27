"""Pinned official containerd candidate, installed only in the owned kind node.

kind starts at 2.2.0. Preserve its fresh control-plane state and follow the
supported sequential minor upgrades through 2.3.7, then 2.4.2. This measures the
exact candidate combination; it is not a broad Kubernetes support claim.
"""
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import tarfile
import time
import tomllib

from gvisor_probe import require

VERSIONS = ("2.3.7", "2.4.2")
DIGESTS = {
    "2.3.7": {
        "arm64": "e08530c6c8a23c612606da5d2832aed9bed8fc7f8c1b41df0f148a701b931d69",
        "amd64": "09a1b669ac6d21e7e24c3c33a9805b48987ba5845e8d88f039c47013a0c0025c",
    },
    "2.4.2": {
        "arm64": "fdfab81a86192ddb8165d72f116ec985485c9e12505e323c4f4579c48020f276",
        "amd64": "35880c1dd4627e8ce56526bfabb39c144e83458ffdb197ce6f05d17d94d7d91e",
    },
}
BINARIES = ("containerd", "containerd-shim-runc-v2", "ctr")


def sha(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def extract(archive, target):
    with tarfile.open(archive, "r:gz") as source:
        members = source.getmembers()
        allowed = {"bin", "bin/containerd-stress", *("bin/" + name for name in BINARIES)}
        require(all(member.name.rstrip("/") in allowed and (member.isfile() or member.isdir()) for member in members),
                "unexpected official containerd bundle member")
        require(all("bin/" + name in {member.name for member in members if member.isfile()} for name in BINARIES),
                "incomplete official containerd bundle")
        source.extractall(target, filter="data")


def prepare(probe, cache=None):
    architecture = probe.run("docker", "version", "--format", "{{.Server.Arch}}").strip()
    require(architecture in ("arm64", "amd64"), "unsupported candidate architecture")
    result = []
    for version in VERSIONS:
        expected = DIGESTS[version].get(architecture)
        require(expected and re.fullmatch(r"[a-f0-9]{64}", expected), "candidate checksum is not frozen")
        filename = "containerd-" + version + "-linux-" + architecture + ".tar.gz"
        url = "https://github.com/containerd/containerd/releases/download/v" + version + "/" + filename
        archive = probe.directory / filename
        cached = Path(cache) / filename if cache else None
        if cached and cached.is_file():
            shutil.copyfile(cached, archive)
        else:
            probe.run("curl", "--fail", "--location", "--silent", "--show-error", "--connect-timeout", "15", "--max-time", "240", "--output", archive, url, timeout=250)
        require(sha(archive) == expected, "candidate containerd bundle SHA256 differs")
        if cached and not cached.exists():
            cached.parent.mkdir(parents=True, exist_ok=True)
            temporary = cached.with_name(cached.name + "." + probe.cluster)
            shutil.copyfile(archive, temporary)
            temporary.replace(cached)
        directory = probe.directory / ("containerd-" + version)
        directory.mkdir()
        extract(archive, directory)
        result.append({"version": version, "architecture": architecture, "url": url, "bundle_sha256": expected,
                       "binaries": {name: sha(directory / "bin" / name) for name in BINARIES}, "directory": directory})
    return result


def install(probe, artifacts):
    owned = json.loads(probe.run("docker", "inspect", probe.node))[0]
    require(owned["Config"]["Labels"].get("io.x-k8s.kind.cluster") == probe.cluster, "candidate node is not owned")
    service = probe.node_run("systemctl", "show", "containerd", "--property=ExecStart", "--value")
    matched = re.search(r"(?:^|[ {])path=([^ ;]+)", service)
    require(matched, "cannot identify actual containerd ExecStart binary")
    binary = PurePosixPath(matched.group(1))
    require(str(binary) in ("/usr/local/bin/containerd", "/usr/bin/containerd"), "unexpected owned-node containerd path")
    paths = probe.node_run("sh", "-c", 'for name in containerd containerd-shim-runc-v2 ctr; do command -v "$name"; done').splitlines()
    require(paths == [str(binary.parent / name) for name in BINARIES], "runtime binaries do not share the actual service directory")
    baseline = probe.node_run("containerd", "--version")
    require(" v2.2.0 " in baseline, "candidate requires the pinned kind containerd 2.2.0 baseline")
    evidence = probe.security["containerd_candidate"] = {"service_execstart": service, "baseline_version": baseline,
        "baseline_binary_sha256": probe.node_run("sha256sum", *paths), "steps": [],
        "scope": "official sequential 2.2.0 -> 2.3.7 -> 2.4.2 only in fresh owned kind node; no host/other cluster changes",
        "kubernetes_support_claim": "exact measured combination only"}
    for artifact in artifacts:
        version = artifact["version"]
        probe.stage("install_owned_containerd_" + version)
        before = probe.node_run("cat", "/etc/containerd/config.toml")
        step = {key: value for key, value in artifact.items() if key != "directory"}
        step.update(config_before=before, cri_before=json.loads(probe.node_run("crictl", "pods", "-o", "json")))
        evidence["steps"].append(step)
        stage = "/opt/agentmoor-containerd-" + version
        probe.node_run("mkdir", "-p", stage)
        require(len(set(probe.node_run("stat", "-c", "%d", stage, str(binary.parent)).splitlines())) == 1,
                "candidate replacement is not on the runtime filesystem")
        for name in BINARIES:
            destination = stage + "/" + name
            probe.run("docker", "cp", artifact["directory"] / "bin" / name, probe.node + ":" + destination)
            require(probe.node_run("sha256sum", destination).split()[0] == artifact["binaries"][name], "copied candidate binary changed")
            probe.node_run("chown", "0:0", destination)
            probe.node_run("chmod", "0755", destination)
            # Atomic rename preserves the executing old inode for live shims;
            # writing over an executing file would fail with ETXTBSY.
            probe.node_run("mv", "-f", destination, str(binary.parent / name))
        probe.node_run("rmdir", stage)
        probe.node_run("systemctl", "restart", "containerd")
        require(probe.node_run("systemctl", "is-active", "containerd").strip() == "active", "candidate service did not restart")
        observed = probe.node_run(str(binary), "--version")
        require(" v" + version + " " in observed, "candidate daemon version differs")
        step["version_observed"] = observed
        step["installed_binary_sha256"] = probe.node_run("sha256sum", *paths)
        require(probe.node_run("cat", "/etc/containerd/config.toml") == before, "candidate unexpectedly changed the saved v2 config")
        loaded = probe.node_run("containerd", "--config", "/etc/containerd/config.toml", "config", "dump")
        tomllib.loads(loaded)
        step["loaded_config_dump"] = loaded
        info = json.loads(probe.node_run("crictl", "info", "-o", "json"))
        require(any(row["type"] == "RuntimeReady" and row["status"] is True for row in info["status"]["conditions"]), "candidate CRI is not RuntimeReady")
        step["cri_info"] = info
        step["cri_after"] = json.loads(probe.node_run("crictl", "pods", "-o", "json"))
        until = time.monotonic() + 30
        while time.monotonic() < until:
            process = subprocess.run([*probe.base, "get", "--raw", "/readyz"], capture_output=True, text=True, timeout=5)
            step["api_ready_observation"] = {"returncode": process.returncode, "stdout": process.stdout, "stderr": process.stderr[-2000:]}
            if process.returncode == 0 and process.stdout.strip() == "ok":
                break
            time.sleep(.2)
        else:
            raise RuntimeError("owned control-plane API did not become ready after supported minor upgrade")
