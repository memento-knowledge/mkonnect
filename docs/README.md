# mkonnect documentation

Guides for administrators deploying and operating the mkonnect on-prem connector.

- **[Installation guide](installation.md)** — deploy the connector with Docker or Helm, verify it
  connected, and upgrade or uninstall it.
- **[Setup guide](setup.md)** — make your internal tools reachable through the connector and attach
  credentials safely.
- **[git-sync guide](git-sync.md)** — optional add-on, installed after the API connector, that
  mirrors selected internal git repositories (branches and tags) to Memento: the order of work and
  requirements, install with Docker or Helm, configure your git server credential, run preflight,
  verify, understand what is and is not copied, the sync schedule, retries and what it keeps on
  disk, upgrade, uninstall, and troubleshoot.
- **[git-sync security and data handling](git-sync-security.md)** — for security reviewers: network
  destinations, data flow, credential custody and revocation, hardening, network policy, and the
  supply chain (image signatures and provenance).

For a conceptual overview, the architecture, and the full configuration reference, see the
[project README](../README.md). Maintainers cutting a release should see
[RELEASING.md](../RELEASING.md).
