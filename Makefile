.PHONY: check fmt build demo test-services test-k8s check-kind-prereqs test-report-consumer test-file-report
.PHONY: test-worker-file-report test-k8s-file-report test-k8s-skills test-k8s-persistent test-k8s-pod-loss
.PHONY: test-k8s-workspace-parts
.PHONY: test-k8s-persistent-faults
.PHONY: test-k8s-v2
.PHONY: test-k8s-gvisor-feasibility
.PHONY: test-k8s-native-guest test-k8s-gvisor-security
.PHONY: test-k8s-sandbox-provider test-k8s-sandbox-quota
.PHONY: test-k8s-sandbox-audit
.PHONY: test-k8s-sandbox-persistent
.PHONY: test-k8s-node-loss
.PHONY: test-sdk-release

check: test-file-report
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests/kubernetes -p test_forward.py -v
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests/kubernetes -p gvisor_probe_test.py -v
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests/kubernetes -p gvisor_security_probe_test.py -v
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests/kubernetes -p sandbox_controller_smoke_test.py -v
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests/kubernetes -p node_loss_probe_test.py -v
	PYTHONDONTWRITEBYTECODE=1 python3 tests/kubernetes/sandbox_provider_model.py --check
	PYTHONDONTWRITEBYTECODE=1 python3 tests/kubernetes/sandbox_provider_skill.py --check
	PYTHONDONTWRITEBYTECODE=1 python3 tests/kubernetes/sandbox_provider_quota.py --check
	PYTHONDONTWRITEBYTECODE=1 python3 tests/kubernetes/sandbox_audit_chain.py --help > /dev/null
	PYTHONDONTWRITEBYTECODE=1 python3 tests/kubernetes/sandbox_persistent_chain.py --help > /dev/null
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/check-sdk-release.py --help > /dev/null
	sh tests/check-kind-prereqs.sh
	@unformatted=$$(gofmt -l cmd internal sdk testdata/sdkconsumer) && test -z "$$unformatted" || { echo "Run make fmt"; exit 1; }
	go vet ./...
	go test -race ./...
	go build ./...

fmt:
	gofmt -w cmd internal sdk testdata/sdkconsumer

build:
	go build -o bin/agentmoor ./cmd/agentmoor

demo: build
	sh examples/demo.sh

test-services:
	sh tests/services.sh

check-kind-prereqs:
	sh scripts/check-kind-prereqs.sh

test-k8s: check-kind-prereqs
	$(MAKE) build
	sh tests/kubernetes/run.sh

test-report-consumer:
	python3 examples/report-consumer/test_ingest.py -v

# Online, cold-cache acceptance of an exact published 0.x module, no local replace.
test-sdk-release:
	@test -n "$(VERSION)" -a -n "$(SDK_RELEASE_OUTPUT)" || { echo "Set VERSION and a new SDK_RELEASE_OUTPUT directory"; exit 2; }
	python3 scripts/check-sdk-release.py "$(VERSION)" --output "$(SDK_RELEASE_OUTPUT)"

test-file-report:
	python3 tests/codex_file_report.py -v

test-worker-file-report:
	go test -count=1 -run '^TestWorkerFileDelivery$$' -v ./cmd/agentmoor

test-k8s-file-report: check-kind-prereqs
	$(MAKE) build
	AGENT=codex sh tests/kubernetes/run.sh --file-report

test-k8s-skills: check-kind-prereqs
	$(MAKE) build
	AGENT=codex sh tests/kubernetes/run.sh --skill-report

test-k8s-persistent: check-kind-prereqs
	$(MAKE) build
	AGENT=codex sh tests/kubernetes/run.sh --persistent

test-k8s-workspace-parts: check-kind-prereqs
	$(MAKE) build
	AGENT=codex sh tests/kubernetes/run.sh --persistent --workspace-parts

# Includes the unchanged production 15-minute lost-Worker deadline.
test-k8s-persistent-faults: check-kind-prereqs
	$(MAKE) build
	AGENT=codex sh tests/kubernetes/run.sh --persistent-faults

# One kind/image: files, pinned Skills, cross-tenant denial, faults and 900s loss.
test-k8s-v2: check-kind-prereqs
	$(MAKE) build
	AGENT=codex sh tests/kubernetes/run.sh --persistent-v2

# Independent runtime feasibility, not AgentMoor/CLI/tenant isolation acceptance.
test-k8s-gvisor-feasibility:
	python3 tests/kubernetes/gvisor_probe.py

# Standalone native guest core; the application provider has its own target.
test-k8s-native-guest:
	python3 tests/kubernetes/native_guest_smoke.py

# Uses the production 15-minute deadline. Keep this separate from the short smoke.
test-k8s-pod-loss: check-kind-prereqs
	$(MAKE) build
	AGENT=codex sh tests/kubernetes/run.sh --pod-loss

# Synthetic actual CNI/resource boundaries; no vendor/provider gate acceptance.
STAGE ?= all
test-k8s-gvisor-security:
	python3 tests/kubernetes/gvisor_security_probe.py --stage $(STAGE)

# Synthetic heartbeat with real kubelet loss; only a new owned kind cluster.
test-k8s-node-loss:
	python3 tests/kubernetes/node_loss_probe.py $(if $(NODE_LOSS_OUTPUT),--output "$(NODE_LOSS_OUTPUT)") $(if $(GVISOR_SECURITY_BUNDLE),--bundle "$(GVISOR_SECURITY_BUNDLE)") $(if $(GVISOR_SECURITY_CALICO_MANIFEST),--calico-manifest "$(GVISOR_SECURITY_CALICO_MANIFEST)") $(if $(GVISOR_SECURITY_CONTAINERD_CACHE),--containerd-cache "$(GVISOR_SECURITY_CONTAINERD_CACHE)")

PROVIDER_SCENARIO ?= both
test-k8s-sandbox-provider:
	@test -n "$(PROVIDER_OUTPUT)" || { echo "Set PROVIDER_OUTPUT to a new evidence file path"; exit 2; }
	python3 tests/kubernetes/sandbox_provider_smoke.py --scenario $(PROVIDER_SCENARIO) --output "$(PROVIDER_OUTPUT)" $(if $(GVISOR_SECURITY_BUNDLE),--bundle "$(GVISOR_SECURITY_BUNDLE)") $(if $(GVISOR_SECURITY_CALICO_MANIFEST),--calico-manifest "$(GVISOR_SECURITY_CALICO_MANIFEST)")

test-k8s-sandbox-quota:
	@test -n "$(QUOTA_OUTPUT)" || { echo "Set QUOTA_OUTPUT to a new evidence file path"; exit 2; }
	python3 tests/kubernetes/sandbox_provider_quota.py --output "$(QUOTA_OUTPUT)" $(if $(GVISOR_SECURITY_BUNDLE),--bundle "$(GVISOR_SECURITY_BUNDLE)") $(if $(GVISOR_SECURITY_CALICO_MANIFEST),--calico-manifest "$(GVISOR_SECURITY_CALICO_MANIFEST)")

# Real isolated Codex execution plus separate operator-only audit inspection.
test-k8s-sandbox-audit:
	@test -n "$(AUDIT_OUTPUT)" || { echo "Set AUDIT_OUTPUT to a new evidence file path"; exit 2; }
	python3 tests/kubernetes/sandbox_audit_chain.py --output "$(AUDIT_OUTPUT)" $(if $(GVISOR_SECURITY_BUNDLE),--bundle "$(GVISOR_SECURITY_BUNDLE)") $(if $(GVISOR_SECURITY_CALICO_MANIFEST),--calico-manifest "$(GVISOR_SECURITY_CALICO_MANIFEST)")

test-k8s-sandbox-persistent:
	@test -n "$(PERSISTENT_OUTPUT)" || { echo "Set PERSISTENT_OUTPUT to a new evidence file path"; exit 2; }
	python3 tests/kubernetes/sandbox_persistent_chain.py --output "$(PERSISTENT_OUTPUT)" $(if $(GVISOR_SECURITY_BUNDLE),--bundle "$(GVISOR_SECURITY_BUNDLE)") $(if $(GVISOR_SECURITY_CALICO_MANIFEST),--calico-manifest "$(GVISOR_SECURITY_CALICO_MANIFEST)")
