"""Observe the first Pod in the pinned Codex diagnostic without performing a render."""
import argparse
import json
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile

BASE = "9c573b9e9ca8725df824fc871fd17eaa6fc51a1d"
HERE = Path(__file__).resolve().parent
STAGES = {"cluster_ready", "image_built", "image_loaded", "namespace_created", "deployments_ready",
          "worker_job_created", "task_submitted"}


def main(check_only):
    with tempfile.TemporaryDirectory(prefix="agentmoor-probe-") as directory:
        root = Path(directory)
        archive = root / "source.tar"
        subprocess.run(["git", "archive", "-o", archive, BASE], cwd=HERE.parents[1], check=True)
        source = root / "source"
        source.mkdir()
        with tarfile.open(archive) as tree:
            tree.extractall(source, filter="data")
        subprocess.run(["git", "apply", "--no-index", "--unidiff-zero", HERE / "file-report-probe.patch"],
                       cwd=source, check=True)
        if check_only:
            for path in ("tests/kubernetes/contract.py", "tests/kubernetes/report_tool.py"):
                compile((source / path).read_bytes(), path, "exec")
            subprocess.run(["sh", "-n", "tests/kubernetes/run.sh"], cwd=source, check=True)
            print("Pinned archive, diagnostic patch and script syntax verified offline.")
            return

        cluster = None
        last_stage = "target_started"
        reports = []
        # The original target owns cluster creation, the private kubeconfig and
        # cleanup. Only the diagnostic's selected fields leave this process.
        with subprocess.Popen(["make", "test-k8s-file-report"], cwd=source, text=True,
                              stdout=subprocess.PIPE, stderr=subprocess.STDOUT) as process:
            for line in process.stdout:
                match = re.match(r'Creating cluster "(agentmoor-[0-9a-f]{12})"', line)
                if match:
                    cluster = match[1]
                    last_stage = "cluster_creation_started"
                    print("Temporary kind cluster", cluster, flush=True)
                stage = line.strip().removeprefix("FILE_REPORT_STAGE ")
                if line.startswith("FILE_REPORT_STAGE ") and stage in STAGES:
                    last_stage = stage
                    print("Observed", stage, flush=True)
                if line.startswith("FILE_REPORT_DIAG "):
                    report = json.loads(line.removeprefix("FILE_REPORT_DIAG "))
                    reports.append(report)
                    print(json.dumps(report, sort_keys=True), flush=True)
            status = process.wait()
        print("make test-k8s-file-report exit", status, flush=True)
        if cluster:
            remaining = subprocess.check_output(
                ["docker", "ps", "-a", "--filter", "label=io.x-k8s.kind.cluster=" + cluster, "--format", "{{.ID}}"],
                text=True,
            ).strip()
            if remaining:
                raise SystemExit("temporary cluster still present: " + cluster)
            print("Temporary cluster removed", cluster)
        if not (cluster and status != 0 and len(reports) == 1 and reports[0]["confirmed"]):
            raise SystemExit(f"first-Pod evidence incomplete; last observed stage: {last_stage}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="verify the diagnostic offline without starting kind")
    main(parser.parse_args().check)
