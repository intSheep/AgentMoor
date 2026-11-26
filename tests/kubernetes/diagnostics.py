"""Best-effort cluster diagnostics which must not replace a contract failure."""
import json
import sys


def dump_failure_logs(kube):
    def logs(resource):
        print("CONTRACT_DIAGNOSTIC " + resource, file=sys.stderr, flush=True)
        try:
            print(kube("logs", resource, "--all-containers=true", "--prefix=true", "--tail=200"),
                  file=sys.stderr, flush=True)
        except Exception as error:
            print("CONTRACT_DIAGNOSTIC_FAILED " + resource + ": " + str(error), file=sys.stderr, flush=True)

    for resource in ("deployment/agentmoor", "deployment/model"):
        logs(resource)
    try:
        pods = json.loads(kube("get", "pods", "-o", "json"))["items"]
    except Exception as error:
        print("CONTRACT_DIAGNOSTIC_FAILED list pods: " + str(error), file=sys.stderr, flush=True)
        return
    for pod in pods:
        if "job-name" in pod["metadata"].get("labels", {}):
            logs("pod/" + pod["metadata"]["name"])
