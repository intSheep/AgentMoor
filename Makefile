.PHONY: check fmt build demo test-services test-k8s check-kind-prereqs test-report-consumer test-file-report
.PHONY: test-worker-file-report test-k8s-file-report test-k8s-skills test-k8s-persistent test-k8s-pod-loss
.PHONY: test-k8s-workspace-parts
.PHONY: test-k8s-persistent-faults
.PHONY: test-k8s-v2
.PHONY: test-k8s-gvisor-feasibility
.PHONY: test-k8s-native-guest test-k8s-gvisor-security
.PHONY: test-k8s-sandbox-provider test-k8s-sandbox-quota

check: test-file-report
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests/kubernetes -p test_forward.py -v
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests/kubernetes -p gvisor_probe_test.py -v
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests/kubernetes -p gvisor_security_probe_test.py -v
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests/kubernetes -p sandbox_controller_smoke_test.py -v
	PYTHONDONTWRITEBYTECODE=1 python3 tests/kubernetes/sandbox_provider_model.py --check
	PYTHONDONTWRITEBYTECODE=1 python3 tests/kubernetes/sandbox_provider_skill.py --check
	PYTHONDONTWRITEBYTECODE=1 python3 tests/kubernetes/sandbox_provider_quota.py --check
	sh tests/check-kind-prereqs.sh
	@unformatted=$$(gofmt -l cmd internal) && test -z "$$unformatted" || { echo "Run make fmt"; exit 1; }
	go vet ./...
	go test -race ./...
	go build ./...

fmt:
	gofmt -w cmd internal

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

PROVIDER_SCENARIO ?= both
test-k8s-sandbox-provider:
	@test -n "$(PROVIDER_OUTPUT)" || { echo "Set PROVIDER_OUTPUT to a new evidence file path"; exit 2; }
	python3 tests/kubernetes/sandbox_provider_smoke.py --scenario $(PROVIDER_SCENARIO) --output "$(PROVIDER_OUTPUT)" $(if $(GVISOR_SECURITY_BUNDLE),--bundle "$(GVISOR_SECURITY_BUNDLE)") $(if $(GVISOR_SECURITY_CALICO_MANIFEST),--calico-manifest "$(GVISOR_SECURITY_CALICO_MANIFEST)")

test-k8s-sandbox-quota:
	@test -n "$(QUOTA_OUTPUT)" || { echo "Set QUOTA_OUTPUT to a new evidence file path"; exit 2; }
	python3 tests/kubernetes/sandbox_provider_quota.py --output "$(QUOTA_OUTPUT)" $(if $(GVISOR_SECURITY_BUNDLE),--bundle "$(GVISOR_SECURITY_BUNDLE)") $(if $(GVISOR_SECURITY_CALICO_MANIFEST),--calico-manifest "$(GVISOR_SECURITY_CALICO_MANIFEST)")
