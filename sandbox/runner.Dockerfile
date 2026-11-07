# Image used for every smoke test (install phase + run phase).
# Build: docker build -f sandbox/runner.Dockerfile -t mcpkg-runner:latest sandbox
FROM node:22-bookworm-slim

# Python + uv for PyPI servers; git for packages that depend on git URLs.
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-venv git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=ghcr.io/astral-sh/uv:0.5.11 /uv /uvx /usr/local/bin/

# Unprivileged user. /work is the per-job volume (owned by runner so named
# volumes inherit the ownership on first mount).
RUN useradd --create-home --uid 10001 runner \
 && mkdir -p /work \
 && chown runner:runner /work
USER runner
WORKDIR /home/runner
ENV NPM_CONFIG_UPDATE_NOTIFIER=false \
    NPM_CONFIG_FUND=false \
    UV_PYTHON_DOWNLOADS=never \
    PYTHONDONTWRITEBYTECODE=1
