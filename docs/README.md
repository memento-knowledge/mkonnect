# mkonnect documentation

Guides for administrators deploying and operating the mkonnect on-prem connector.

- **[Installation guide](installation.md)** — deploy the connector with Docker or Helm, verify it
  connected, and upgrade or uninstall it.
- **[Setup guide](setup.md)** — make your internal tools reachable through the connector and attach
  credentials safely.
- **[git-sync guide](git-sync.md)** — optional add-on that mirrors selected internal git
  repositories (branches and tags) to Memento: install with Docker or Helm, configure your git
  server credential, verify, understand the sync schedule, retries and what it keeps on disk,
  upgrade, uninstall, and troubleshoot.
- **[git-sync security and data handling](git-sync-security.md)** — for security reviewers: network
  destinations, data flow, credential custody, hardening, and network policy.

For a conceptual overview, the architecture, and the full configuration reference, see the
[project README](../README.md). Maintainers cutting a release should see
[RELEASING.md](../RELEASING.md).
