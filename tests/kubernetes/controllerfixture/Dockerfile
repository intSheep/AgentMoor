# Synthetic controller guest only: no Codex, vendor CLI or production gateway.
FROM docker.io/library/busybox@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
COPY controllerfixture /usr/local/bin/agentmoor
COPY ca.pem /etc/agentmoor/fixture-ca.pem
RUN printf 'synthetic-public-sentinel' > /etc/agentmoor/readonly-sentinel \
    && chmod 0666 /etc/agentmoor/readonly-sentinel \
    && printf '#!/bin/sh\nexit 97\n' > /usr/local/bin/fixture-native-disabled \
    && chmod 0555 /usr/local/bin/fixture-native-disabled
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/agentmoor"]
