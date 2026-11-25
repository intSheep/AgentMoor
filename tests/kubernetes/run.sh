#!/bin/sh
# Disposable cluster and kubeconfig. Never change the user's current context.
set -eu
cd "$(dirname "$0")/../.."
state=$(mktemp -d)
name="agentmoor-$(python3 -c 'import uuid; print(uuid.uuid4().hex[:12])')"
agents=${AGENT:-"codex gemini-cli opencode claude-code"}
images=""
export KUBECONFIG="$state/kubeconfig"
cleanup() {
    kind delete cluster --name "$name" --kubeconfig "$KUBECONFIG"
    for image in $images; do docker image rm "$image" >/dev/null 2>&1 || true; done
    rm -rf "$state"
}
trap cleanup EXIT
kind create cluster --image kindest/node:v1.35.0 --name "$name" --kubeconfig "$KUBECONFIG" --wait 2m
for agent in $agents; do
    image="agentmoor-contract:$name-$agent"
    images="$images $image"
    echo "Running Kubernetes contract for $agent"
    docker build -f tests/kubernetes/Dockerfile --build-arg AGENT="$agent" -t "$image" .
    kind load docker-image "$image" --name "$name"
    python3 tests/kubernetes/contract.py --adapter "$agent" --context "kind-$name" --kubeconfig "$KUBECONFIG" --image "$image"
done
