# Deploy

Deployment artifacts for the agent and the command center live here.

- `speedtest-agent.service` is the systemd unit template for the node agent. It
  runs the agent as a dedicated, unprivileged `speedtest-agent` user with
  `Restart=always`, and forwards the agent's structured JSON logs to the
  journal. Copy it to `/etc/systemd/system/` and adjust `ExecStart` to the
  installed binary. Update `ReadWritePaths` if the credential directory is
  changed. Enroll as that same user (for example
  `sudo -u speedtest-agent speedtest-agent enroll --center <url> --token <token> --name <name>`)
  so the 0600 credential file is owned by it; otherwise `chown speedtest-agent
  /etc/speedtest-agent/credential` after enrolling.
- Nothing in this directory may contain a secret. The enrollment token is
  supplied to `agent enroll` and the resulting credential is stored in the
  configured 0600 credential file.
- The agent and the center are deployed independently, matching the rule that
  nodes know only their own configuration.
