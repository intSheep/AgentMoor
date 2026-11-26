.PHONY: check fmt build demo test-services test-k8s check-kind-prereqs test-report-consumer test-file-report
.PHONY: test-worker-file-report test-k8s-file-report test-k8s-skills

check: test-file-report
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
