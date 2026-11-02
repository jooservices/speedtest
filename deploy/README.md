# Deploy

Deployment artifacts for the agent and the command center live here. A later
ticket adds the service definitions (for example systemd units), release
scripts, and any environment-specific templates.

- Nothing in this directory may contain a secret; credentials come from `.env`.
- The agent and the center are deployed independently, matching the rule that
  nodes know only their own configuration.
