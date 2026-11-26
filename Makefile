.PHONY: check fmt build demo test-services test-k8s check-kind-prereqs test-report-consumer test-file-report
.PHONY: test-worker-file-report test-k8s-file-report test-k8s-skills test-k8s-persistent test-k8s-pod-loss
.PHONY: test-k8s-workspace-parts
.PHONY: test-k8s-persistent-faults
.PHONY: test-k8s-v2

check: test-file-report
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests/kubernetes -p test_forward.py -v
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

# Uses the production 15-minute deadline. Keep this separate from the short smoke.
test-k8s-pod-loss: check-kind-prereqs
	$(MAKE) build
	AGENT=codex sh tests/kubernetes/run.sh --pod-loss
