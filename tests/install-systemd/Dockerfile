FROM ubuntu:24.04

ENV container=docker

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl dbus passwd python3 systemd systemd-sysv tar util-linux \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

STOPSIGNAL SIGRTMIN+3
CMD ["/lib/systemd/systemd", "--system", "--log-target=console", "--log-level=debug"]
