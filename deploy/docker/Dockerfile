# Speedtest staging image: the complete product (command center + agents) on Ubuntu.
# Binaries come from `make release` (dist/); the engine is the Ubuntu speedtest-cli package, and the
# entrypoint migrates, bootstraps and supervises everything.
FROM ubuntu:24.04

# speedtest-cli (the Python tool from Ubuntu) is the engine used on staging; Ookla's CLI can be added later by
# building with --build-arg OOKLA_VERSION/OOKLA_SHA256 and switching a node's engine to ookla.
ARG OOKLA_VERSION=""
ARG OOKLA_SHA256=""

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl tini postgresql-client jq procps tzdata speedtest-cli \
 && rm -rf /var/lib/apt/lists/* \
 && speedtest-cli --version | head -1

RUN if [ -n "$OOKLA_VERSION" ]; then \
      curl -fsSL -o /tmp/ookla.tgz "https://install.speedtest.net/app/cli/ookla-speedtest-${OOKLA_VERSION}-linux-x86_64.tgz" \
      && echo "${OOKLA_SHA256}  /tmp/ookla.tgz" | sha256sum -c - \
      && tar -xzf /tmp/ookla.tgz -C /usr/local/bin speedtest && rm -f /tmp/ookla.tgz && speedtest --version; \
    fi

RUN useradd --system --create-home --home-dir /var/lib/speedtest --shell /usr/sbin/nologin speedtest

COPY dist/speedtest-center-linux-amd64  /usr/local/bin/speedtest-center
COPY dist/speedtest-agent-linux-amd64   /usr/local/bin/speedtest-agent
COPY dist/speedtest-migrate-linux-amd64 /usr/local/bin/speedtest-migrate
COPY deploy/docker/entrypoint.sh        /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/speedtest-center /usr/local/bin/speedtest-agent /usr/local/bin/speedtest-migrate /usr/local/bin/entrypoint.sh

USER speedtest
WORKDIR /var/lib/speedtest
VOLUME ["/var/lib/speedtest"]
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
