# mcpkg application image (web, scheduler and workers all use it).
# Build from the repo root: docker build -f deploy/Dockerfile -t mcpkg:latest .
# Multi-arch (core = t4g/arm64, workers = amd64): the Go stage runs natively and cross-compiles.
#   docker buildx build --platform linux/amd64,linux/arm64 -f deploy/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.24-bookworm AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/mcpkg ./cmd/mcpkg

FROM debian:bookworm-slim
ARG TARGETARCH
ARG DOCKER_CLI_VERSION=27.3.1
ARG OSV_SCANNER_VERSION=2.0.2
ARG WITH_SEMGREP=false
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git curl python3 \
 && rm -rf /var/lib/apt/lists/*
# Docker CLI only (the daemon is the host's, via the mounted socket) for sandboxed smoke tests.
RUN set -eux; arch="$(case "$TARGETARCH" in arm64) echo aarch64;; *) echo x86_64;; esac)"; \
    curl -fsSL "https://download.docker.com/linux/static/stable/${arch}/docker-${DOCKER_CLI_VERSION}.tgz" \
    | tar -xz --strip-components=1 -C /usr/local/bin docker/docker
# Optional scanners: the pipeline uses them when present and skips them otherwise.
RUN (curl -fsSL -o /usr/local/bin/osv-scanner \
      "https://github.com/google/osv-scanner/releases/download/v${OSV_SCANNER_VERSION}/osv-scanner_linux_${TARGETARCH:-amd64}" \
     && chmod +x /usr/local/bin/osv-scanner) || echo "osv-scanner not installed; continuing without it"
RUN if [ "$WITH_SEMGREP" = "true" ]; then \
      apt-get update && apt-get install -y --no-install-recommends python3-pip && \
      pip3 install --break-system-packages semgrep && rm -rf /var/lib/apt/lists/*; fi
RUN useradd --system --uid 10001 --home-dir /app --shell /usr/sbin/nologin mcpkg
WORKDIR /app
COPY --from=build /out/mcpkg /usr/local/bin/mcpkg
COPY pipeline.json ./pipeline.json
COPY hooks ./hooks
COPY scanners ./scanners
ENV PIPELINE_FILE=/app/pipeline.json SEMGREP_RULES=/app/scanners/semgrep/mcp-rules.yml WORK_DIR=/tmp LOG_DIR=-
# Unprivileged by default; compose runs worker-heavy as root because it needs the Docker socket.
USER 10001
EXPOSE 8080
ENTRYPOINT ["mcpkg"]
CMD ["serve"]
