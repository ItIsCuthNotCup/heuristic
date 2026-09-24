FROM golang:1.27.1-trixie@sha256:433790e515d27dc6003e847e644cc0af956985cf315c1c58a3b73ee2dd305183 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY harness/ harness/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -o /out/heuristic ./cmd/heuristic \
    && CGO_ENABLED=0 go build -trimpath -buildvcs=false -o /out/heu ./cmd/heu

# Debian 13.7 (trixie), image build 2026-09-18.
FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a
RUN apt-get update && apt-get install -y --no-install-recommends bash ca-certificates tini \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /workspace /home/agent \
    && chown 10001:10001 /workspace /home/agent
COPY --from=build /out/heuristic /usr/local/bin/heuristic
COPY --from=build /out/heu /usr/local/bin/heu
COPY LICENSE /usr/share/doc/heuristic/LICENSE
ENV HOME=/home/agent SHELL=/bin/bash
USER 10001:10001
WORKDIR /workspace
ENTRYPOINT ["/usr/bin/tini", "--", "heuristic"]
