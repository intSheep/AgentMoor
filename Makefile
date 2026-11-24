.PHONY: check fmt build demo test-services test-k8s test-report-consumer

check:
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

test-k8s: build
	sh tests/kubernetes/run.sh

test-report-consumer:
	python3 examples/report-consumer/test_ingest.py -v
