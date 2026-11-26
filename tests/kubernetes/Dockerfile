FROM golang:1.27.4-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN go build -trimpath -o /agentmoor ./cmd/agentmoor
COPY tests/kubernetes/faultproxy ./tests/kubernetes/faultproxy
RUN go build -trimpath -o /faultproxy ./tests/kubernetes/faultproxy/cmd/faultproxy

FROM python:3.13-slim-bookworm AS fixture
ARG AGENT=claude-code
COPY tests/kubernetes/install_native.py /opt/test/install_native.py
RUN python3 /opt/test/install_native.py "$AGENT"
COPY --from=build /agentmoor /usr/local/bin/agentmoor
COPY --from=build /faultproxy /opt/test/faultproxy
COPY tests/kubernetes/native_count.sh /opt/test/native_count.sh
COPY tests/fixture_provider.py tests/kubernetes/model.py /opt/test/
COPY tests/kubernetes/file_model.py tests/kubernetes/report_tool.py examples/file-report/summarize.py /opt/test/
COPY tests/kubernetes/skill_model.py tests/kubernetes/skill_tool.py /opt/test/
ENV PYTHONDONTWRITEBYTECODE=1
USER 65532:65532
CMD ["agentmoor", "--help"]
