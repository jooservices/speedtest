# Speedtest production image: the command center only (agents run on the nodes). Binaries come from `make release`.
FROM ubuntu:24.04
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl tini tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --create-home --home-dir /var/lib/speedtest --shell /usr/sbin/nologin speedtest
COPY dist/speedtest-center-linux-amd64  /usr/local/bin/speedtest-center
COPY dist/speedtest-migrate-linux-amd64 /usr/local/bin/speedtest-migrate
COPY deploy/prod/entrypoint.sh          /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/speedtest-center /usr/local/bin/speedtest-migrate /usr/local/bin/entrypoint.sh
USER speedtest
WORKDIR /var/lib/speedtest
VOLUME ["/var/lib/speedtest"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD curl -fsS "http://127.0.0.1:${SPEEDTEST_PORT:-18090}/readyz" >/dev/null || exit 1
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
