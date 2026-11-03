# Deploy

Deployment artifacts for the node agent and the command center live here.

- `speedtest-agent.service` is the systemd **system** unit template for the
  node agent. It runs the agent as a dedicated, unprivileged `speedtest-agent`
  user with `Restart=always`, and forwards the agent's structured JSON logs to
  the journal. Copy it to `/etc/systemd/system/` and adjust `ExecStart` to the
  installed binary. Update `ReadWritePaths` if the credential directory is
  changed. Enroll as that same user (for example
  `sudo -u speedtest-agent speedtest-agent enroll --center <url> --token <token> --name <name>`)
  so the 0600 credential file is owned by it; otherwise `chown speedtest-agent
  /etc/speedtest-agent/credential` after enrolling.
- `speedtest-center.service` is the systemd **user** unit template for the
  command center. It runs the center as the deploying user with
  `Restart=always`, reads its configuration from an `EnvironmentFile=`, and
  forwards the center's structured JSON logs to the journal. Copy it to
  `~/.config/systemd/user/`, then `systemctl --user daemon-reload` and
  `systemctl --user enable --now speedtest-center`. Adjust `WorkingDirectory`,
  `EnvironmentFile` and `ExecStart` if the release directory is changed; keep
  the `.env` in that directory so migrations (`speedtest-migrate up`) and the
  service agree on the database. See [docs/deployment.md](../docs/deployment.md).
- `speedtest-center.nginx.conf` is an example reverse proxy for the center. The
  center speaks plain HTTP on loopback and must sit behind TLS; this template
  terminates TLS, forwards the host and client address headers, and redirects
  plain HTTP to HTTPS. Substitute the `server_name`, certificate paths and
  upstream address for your deployment.
- Nothing in this directory may contain a secret. The enrollment token is
  supplied to `agent enroll` and the resulting credential is stored in the
  configured 0600 credential file; the center's database password lives only in
  its mode-0600 `.env`.
- The agent and the center are deployed independently, matching the rule that
  nodes know only their own configuration.
