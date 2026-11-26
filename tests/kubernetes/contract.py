"""Deploy the SaaS contract in a fresh namespace on an explicitly selected cluster."""
import argparse
import contextlib
import json
from pathlib import Path
import re
import secrets
import select
import ssl
import subprocess
import sys
import tempfile
import time
import uuid

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
from saas_contract import ReportClient
from saas_file_contract import FileReportClient
from file_report_contract import run as run_file_report
from pod_loss_contract import run as run_pod_loss
from skill_report_contract import SkillCatalog, SkillReportClient, run as run_skill_report
from runtimes import BINARIES, environment
from persistent_contract import PersistentContract


def check(*args, data=None):
    return subprocess.check_output(list(map(str, args)), input=data, text=True, timeout=180).strip()


def main(opts, temp):
    if opts.pod_loss or opts.skill_report:
        opts.file_report = True
    if opts.backend == "postgres-s3" and (opts.pod_loss or not opts.file_report):
        raise ValueError("PostgreSQL/S3 acceptance requires --file-report or --skill-report; Pod-loss is a separate local-backend contract")
    ns = "agentmoor-contract-" + uuid.uuid4().hex[:10]
    base = ["kubectl", "--context", opts.context, "--kubeconfig", opts.kubeconfig]
    def kube(*args, data=None):
        return check(*base, "-n", ns, *args, data=data)

    scope = {"tenant": "saas", "project": "reports", "runtime": "report-job", "adapter": opts.adapter}
    config = {"endpoint": "https://agentmoor:7401", "scope": scope,
              "workspace": "/run/task/work", "state_dir": "/run/task/state",
              "runtime_cli": "/usr/local/bin/" + BINARIES[opts.adapter]}
    if opts.file_report and opts.adapter != "codex":
        raise ValueError("the file tool contract requires Codex")
    cfg = temp / "worker.json"
    cfg.write_text(json.dumps(config))
    platform = temp / "platform"
    binary = HERE.parents[1] / "bin/agentmoor"
    check(binary, "init", "--data", platform)
    def issue(*rights, allocation=scope):
        flags = [part for key, val in allocation.items() for part in ("--" + key, val)]
        return check(binary, "issue", "--data", platform, *flags, *rights)

    cert, key = temp / "cert.pem", temp / "key.pem"
    conf = temp / "openssl.cnf"
    conf.write_text("[req]\ndistinguished_name=dn\nx509_extensions=ext\nprompt=no\n[dn]\nCN=agentmoor\n"
                    "[ext]\nsubjectAltName=DNS:agentmoor,IP:127.0.0.1\nbasicConstraints=critical,CA:TRUE\n")
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                    "-keyout", key, "-out", cert, "-config", conf], check=True, stderr=subprocess.DEVNULL)
    def manifest_for(name, credential, command, config_name="configuration"):
        manifest = (HERE / "job.yaml").read_text().replace("CONTRACT_IMAGE", opts.image)
        manifest = manifest.replace("JOB_CONFIG", config_name).replace("JOB_NAME", name).replace("JOB_CREDENTIAL", credential)
        native_env = environment(opts.adapter)
        if opts.file_report:
            native_env["CODEX_FILE_TOOL"] = "/opt/test/skill_tool.py" if opts.skill_report else "/opt/test/report_tool.py"
        extra = [{"name": key, "value": value} for key, value in native_env.items()]
        manifest = manifest.replace("NATIVE_ENV", "\n".join("        - " + json.dumps(var) for var in extra))
        return manifest.replace("JOB_COMMAND", json.dumps(command))

    def apply_job(*args, **kwargs):
        kube("apply", "-f", "-", data=manifest_for(*args, **kwargs))

    storage = None
    cancellation_scope = {**scope, "runtime": "cancel-job"}
    check(*base, "create", "namespace", ns)
    try:
        if opts.backend == "postgres-s3":
            kube("apply", "-f", HERE / "persistent.yaml")
            storage = PersistentContract(kube, base, ns, forward)
            storage.initialize()
        kube("create", "secret", "generic", "signing", "--from-file=" + str(platform / "signing.key"))
        kube("create", "secret", "generic", "tls", "--from-file=" + str(cert), "--from-file=" + str(key))
        kube("create", "configmap", "configuration", "--from-file=" + str(cfg), "--from-file=" + str(cert))
        cancel_cfg = temp / "cancel-worker.json"
        cancel_cfg.write_text(json.dumps({**config, "scope": cancellation_scope}))
        kube("create", "configmap", "cancel-configuration", "--from-file=worker.json=" + str(cancel_cfg), "--from-file=" + str(cert))
        for name, rights, allocation in (("executor", ["--execute"], scope),
                                         ("cancel-executor", ["--execute"], cancellation_scope)):
            token = temp / name
            token.write_text(issue(*rights, allocation=allocation))
            kube("create", "secret", "generic", name, "--from-file=token=" + str(token))
        kube("apply", "-f", HERE.parents[1] / "deploy/kubernetes/turn-rbac.yaml")
        template = json.loads(kube("create", "--dry-run=client", "-f", "-", "-o", "json",
                                   data=manifest_for("template", "executor", ["agentmoor"])))
        profiles = temp / "profiles.json"
        profiles.write_text(json.dumps([{"scope": scope, "pod_spec": template["spec"]["template"]["spec"]}]))
        kube("create", "configmap", "turn-profiles", "--from-file=" + str(profiles))
        workloads = (HERE / "workloads.yaml").read_text().replace("CONTRACT_IMAGE", opts.image)
        if opts.file_report:
            workloads = workloads.replace("/opt/test/model.py", "/opt/test/skill_model.py" if opts.skill_report else "/opt/test/file_model.py")
        if storage:
            native_env = "        env:\n" + "\n".join("        - " + json.dumps(item) for item in storage.api_environment()) + "\n"
            workloads = workloads.replace("        ports: [{containerPort: 7401}]", native_env + "        ports: [{containerPort: 7401}]")
        kube("apply", "-f", "-", data=workloads)
        for deploy in ("agentmoor", "model"):
            kube("rollout", "status", "deployment/" + deploy, "--timeout=150s")
        with forward(base, ns) as endpoint:
            context = ssl.create_default_context(cafile=str(cert))
            if opts.skill_report:
                catalog = SkillCatalog(endpoint, issue("--write"), context)
                config["skill_pins"] = [catalog.pin]
                kube("patch", "configmap", "configuration", "--type=merge", "-p",
                     json.dumps({"data": {"worker.json": json.dumps(config)}}))
                client = SkillReportClient(endpoint, issue("--submit"), scope["runtime"], context, catalog=catalog)
            else:
                client_class = FileReportClient if opts.file_report else ReportClient
                client = client_class(endpoint, issue("--submit"), scope["runtime"], context)
            apply_job("produce", "executor", ["agentmoor", "run-task", "--config", "/config/worker.json"])
            if opts.pod_loss:
                run_pod_loss(client, kube, temp, cert, wait_job)
                return
            if opts.skill_report:
                negative = (issue("--submit", allocation=cancellation_scope),
                            lambda: apply_job("no-skill", "cancel-executor",
                                              ["agentmoor", "run-task", "--config", "/config/worker.json"],
                                              config_name="cancel-configuration"))
                run_skill_report(client, catalog, kube, temp, cert, wait_job, negative,
                                 queued_restart=storage.restart_queued_turn if storage else None)
                if storage:
                    storage.verify("skill-report")
                return
            if opts.file_report:
                negative = (issue("--submit", allocation=cancellation_scope),
                            lambda: apply_job("missing-input", "cancel-executor",
                                              ["agentmoor", "run-task", "--config", "/config/worker.json"],
                                              config_name="cancel-configuration"))
                run_file_report(client, kube, temp, cert, wait_job, negative,
                                queued_restart=storage.restart_queued_turn if storage else None)
                if storage:
                    storage.verify("file-report")
                return
            client.submit()
            try:
                first = client.await_result()
            except Exception:
                print(kube("logs", "job/produce"), file=sys.stderr)
                raise
            wait_job(kube, "produce")
            kube("delete", "job", "produce", "--cascade=foreground", "--wait=true")
            if json.loads(kube("get", "pods", "-l", "job-name=produce", "-o", "json"))["items"]:
                raise AssertionError("source Pod still exists")
            assert first == client.await_result()
            print("SaaS verified the versioned report after deletion of the original Pod and emptyDir.", flush=True)
            initial = json.loads(client.request("GET", client.route))
            parents = {r["kind"]: r for r in initial["assets"]}
            session = parents["session"]["id"]
            nonce, check_code = secrets.token_hex(12), secrets.token_hex(12)
            expected = {"totals_cents": {client.region: client.amount - 7, "west": 31}, "confirmation": nonce}
            receipts = []
            for message, answer in [
                    ("Repeat the totals from the original CSV. Confirmation " + nonce, expected),
                    ("Recall the last confirmation and totals. Check " + check_code, {**expected, "check": check_code})]:
                turn = client.ask(session, message)
                label = "agentmoor.io/turn=" + turn
                try:
                    row, reply = client.answer(turn)
                    assert reply == answer, (reply, answer)
                    assert row["parent"]["workspace"] == parents["workspace"]
                    assert row["parent"]["session"] == parents["session"]
                except Exception:
                    print(kube("get", "jobs,pods", "-o", "wide"), file=sys.stderr)
                    print(kube("logs", "deployment/agentmoor"), file=sys.stderr)
                    for pod in json.loads(kube("get", "pods", "-l", label, "-o", "json"))["items"]:
                        print(kube("logs", "pod/" + pod["metadata"]["name"]), file=sys.stderr)
                    raise
                parents = {r["kind"]: r for r in row["assets"]}
                receipts.append((turn, reply))
                jobs = json.loads(kube("get", "jobs", "-l", label, "-o", "json"))["items"]
                assert len(jobs) == 1
                name = jobs[0]["metadata"]["name"]
                wait_job(kube, name)
                kube("delete", "job", name, "--cascade=foreground", "--wait=true")
                assert not json.loads(kube("get", "pods", "-l", label, "-o", "json"))["items"]
            assert all(client.answer(turn)[1] == reply for turn, reply in receipts)
            assert first == client.await_result()
            print("SaaS recovered two consecutive turns from automatically created Jobs and read both replies after Pod deletion.", flush=True)
            committed = json.loads(client.request("GET", client.route + "/checkpoint"))
            blocked = client.ask(session, "Hold this follow-up until cancelled.")
            blocked_route = client.route + "/turns/" + blocked
            await_model(kube, "holding follow-up request")
            begin = time.monotonic()
            assert json.loads(client.request("POST", blocked_route + "/cancel"))["state"] == "cancelling"
            jobs = json.loads(kube("get", "jobs", "-l", "agentmoor.io/turn=" + blocked, "-o", "json"))["items"]
            assert len(jobs) == 1
            wait_job(kube, jobs[0]["metadata"]["name"], seconds=25)
            stopped = json.loads(client.request("GET", blocked_route))
            assert time.monotonic() - begin < 25 and stopped["state"] == "cancelled" and not stopped.get("assets")
            assert committed == json.loads(client.request("GET", client.route + "/checkpoint"))
            print("SaaS cancelled an active follow-up and retained the last committed conversation.", flush=True)
            cancellation = ReportClient(endpoint, issue("--submit", allocation=cancellation_scope),
                                        cancellation_scope["runtime"], client.ssl_context)
            cancellation.spec["instruction"] = "Wait for cancellation in the contract test."
            apply_job("cancel", "cancel-executor", ["agentmoor", "run-task", "--config", "/config/worker.json"],
                      config_name="cancel-configuration")
            cancellation.submit()
            await_model(kube, "holding native request")
            cancel_start = time.monotonic()
            pending = json.loads(cancellation.request("POST", cancellation.route + "/cancel"))
            assert pending["state"] == "cancelling"
            wait_job(kube, "cancel", seconds=25)
            stopped = json.loads(cancellation.request("GET", cancellation.route))
            elapsed = time.monotonic() - cancel_start
            assert elapsed < 25, f"cancellation took {elapsed:.1f} seconds"
            assert stopped["state"] == "cancelled" and not stopped.get("assets")
            print(f"SaaS cancelled an active native turn; worker acknowledged and Job exited in {elapsed:.1f}s.", flush=True)
    finally:
        check(*base, "delete", "namespace", ns, "--wait=false")


def await_model(kube, marker):
    deadline = time.monotonic() + 65
    while time.monotonic() < deadline:
        if marker in kube("logs", "deployment/model"):
            return
        time.sleep(0.5)
    raise AssertionError("expected native generation to reach " + marker)


def wait_job(kube, name, seconds=170):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        status = json.loads(kube("get", "job", name, "-o", "json")).get("status", {})
        if status.get("succeeded") == 1:
            return
        if status.get("failed"):
            raise RuntimeError(name + " failed: " + kube("logs", "job/" + name))
        time.sleep(0.7)
    raise TimeoutError(name + " timed out: " + kube("get", "pods", "-o", "wide"))


class ForwardedEndpoint(str):
    """Stable local origin; infrastructure can reconnect after a service Pod restart."""


@contextlib.contextmanager
def forward(base, ns, service="agentmoor", port=7401, scheme="https"):
    errors = tempfile.TemporaryFile(mode="w+")
    proc = None

    def start(local=""):
        process = subprocess.Popen([*base, "-n", ns, "port-forward", "svc/" + service, str(local) + ":" + str(port),
                                    "--address", "127.0.0.1"], stdout=subprocess.PIPE, stderr=errors, text=True)
        try:
            if not select.select([process.stdout], [], [], 15)[0]:
                raise TimeoutError("port-forward did not start")
            match = re.search(r"127\.0\.0\.1:(\d+)", process.stdout.readline())
            if not match:
                raise RuntimeError("no forwarded port")
            return process, match[1]
        except Exception:
            if process.poll() is None:
                process.terminate()
            process.wait(timeout=10)
            process.stdout.close()
            raise

    def stop():
        if proc is not None:
            if proc.poll() is None:
                proc.terminate()
            proc.wait(timeout=10)
            proc.stdout.close()

    try:
        proc, local = start()
        endpoint = ForwardedEndpoint(scheme + "://127.0.0.1:" + local)

        def reconnect():
            nonlocal proc
            stop()
            proc, actual = start(local)
            assert actual == local

        endpoint.reconnect = reconnect
        yield endpoint
    finally:
        stop()
        if sys.exc_info()[0] is not None:
            errors.seek(0)
            print(errors.read()[-1200:], file=sys.stderr)
        errors.close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--adapter", choices=BINARIES, required=True)
    parser.add_argument("--file-report", action="store_true")
    special = parser.add_mutually_exclusive_group()
    special.add_argument("--pod-loss", action="store_true")
    special.add_argument("--skill-report", action="store_true")
    parser.add_argument("--backend", choices=("local", "postgres-s3"), default="local")
    for key in ("context", "kubeconfig", "image"):
        parser.add_argument("--" + key, required=True)
    with tempfile.TemporaryDirectory(prefix="agentmoor-saas-") as tmp:
        main(parser.parse_args(), Path(tmp))
