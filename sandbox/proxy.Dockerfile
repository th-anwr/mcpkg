# Egress allowlist proxy for the smoke-test install phase.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends squid && rm -rf /var/lib/apt/lists/*
COPY squid.conf /etc/squid/squid.conf
RUN mkdir -p /var/log/squid && chown -R proxy:proxy /var/log/squid \
 && squid -k parse -f /etc/squid/squid.conf
EXPOSE 3128
CMD ["squid", "-N", "-f", "/etc/squid/squid.conf"]
