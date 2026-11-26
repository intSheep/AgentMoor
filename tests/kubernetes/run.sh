#!/bin/sh
# Disposable cluster and kubeconfig. Never change the user's current context.
set -eu
cd "$(dirname "$0")/../.."
agents=${AGENT:-"codex gemini-cli opencode claude-code"}
images=""
persistent=0
persistent_faults=0
persistent_v2=0
if [ "${1:-}" = "--persistent-v2" ]; then
    persistent=1
    persistent_v2=1
    shift
    if [ "$agents" != "codex" ] || [ "$#" -ne 0 ]; then
        echo "V2 contract requires AGENT=codex and no extra flags" >&2
        exit 1
    fi
elif [ "${1:-}" = "--persistent-faults" ]; then
    persistent=1
    persistent_faults=1
    shift
    if [ "$agents" != "codex" ] || [ "$#" -ne 0 ]; then
        echo "Persistent faults require AGENT=codex and no extra flags" >&2
        exit 1
    fi
elif [ "${1:-}" = "--persistent" ]; then
    persistent=1
    shift
    if [ "$agents" != "codex" ] || [ "$#" -gt 1 ] || { [ "$#" -eq 1 ] && [ "$1" != "--workspace-parts" ]; }; then
        echo "Persistent contract requires AGENT=codex and optionally --workspace-parts" >&2
        exit 1
    fi
fi
if [ "$persistent" -eq 1 ]; then sh tests/kubernetes/check_persistent_prereqs.sh; fi
state=$(mktemp -d)
name="agentmoor-$(python3 -c 'import uuid; print(uuid.uuid4().hex[:12])')"
export KUBECONFIG="$state/kubeconfig"
cleanup() {
    kind delete cluster --name "$name" --kubeconfig "$KUBECONFIG"
    for image in $images; do docker image rm "$image" >/dev/null 2>&1 || true; done
    rm -rf "$state"
}
trap cleanup EXIT
kind create cluster --image kindest/node:v1.35.0 --name "$name" --kubeconfig "$KUBECONFIG" --wait 2m
if [ "$persistent" -eq 1 ]; then
    for service in postgres:18-alpine pgsty/minio:RELEASE.2026-04-17T00-00-00Z; do
        docker image inspect "$service" >/dev/null 2>&1 || docker pull "$service"
        # Docker's containerd store can retain a multi-platform index without
        # every referenced architecture. Export just the installed architecture
        # so kind's all-platform import never follows missing index entries.
        platform=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$service")
        archive="$state/$(echo "$service" | tr '/:' '__').tar"
        docker image save --platform "$platform" -o "$archive" "$service"
        kind load image-archive "$archive" --name "$name"
    done
fi
for agent in $agents; do
    image="agentmoor-contract:$name-$agent"
    images="$images $image"
    echo "Running Kubernetes contract for $agent"
    docker build -f tests/kubernetes/Dockerfile --build-arg AGENT="$agent" -t "$image" .
    kind load docker-image "$image" --name "$name"
    if [ "$persistent_v2" -eq 1 ]; then
        for scenario in --file-report --skill-report; do
            python3 tests/kubernetes/contract.py --adapter "$agent" --context "kind-$name" --kubeconfig "$KUBECONFIG" --image "$image" --backend postgres-s3 --workspace-parts "$scenario"
        done
        python3 tests/kubernetes/contract.py --adapter "$agent" --context "kind-$name" --kubeconfig "$KUBECONFIG" --image "$image" --backend postgres-s3 --pod-loss --workspace-parts --publication-faults
    elif [ "$persistent_faults" -eq 1 ]; then
        python3 tests/kubernetes/contract.py --adapter "$agent" --context "kind-$name" --kubeconfig "$KUBECONFIG" --image "$image" --backend postgres-s3 --pod-loss --workspace-parts --publication-faults
    elif [ "$persistent" -eq 1 ]; then
        for scenario in --file-report --skill-report; do
            python3 tests/kubernetes/contract.py --adapter "$agent" --context "kind-$name" --kubeconfig "$KUBECONFIG" --image "$image" --backend postgres-s3 "$scenario" "$@"
        done
    else
        python3 tests/kubernetes/contract.py --adapter "$agent" --context "kind-$name" --kubeconfig "$KUBECONFIG" --image "$image" "$@"
    fi
done
