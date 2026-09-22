# mkonnect

[![CI](https://github.com/memento-knowledge/mkonnect/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/memento-knowledge/mkonnect/actions/workflows/ci.yml)

Lightweight on-prem connector for the Memento Knowledge (mk) platform. It runs inside a customer's network and bridges internal tools — Jenkins, Prometheus, Coralogix, and more — to the Memento platform over a secure WebSocket tunnel, so nothing needs to be exposed to the public internet.

## Documentation

Administrator guides live in [`docs/`](docs/):

- **[Installation guide](docs/installation.md)** — deploy with Docker or Helm, verify the connection, upgrade, and uninstall.
- **[Setup guide](docs/setup.md)** — make your internal tools reachable and attach credentials safely.

The rest of this README is a conceptual overview and configuration reference.

## How it works

```
Memento platform ── wss:// ──▶ Bridge Gateway ── WebSocket ──▶ mkonnect ── HTTP ──▶ Jenkins / Prometheus / ...
```

1. **Connect.** mkonnect dials the Bridge Gateway over `wss://`. The gateway sends a random challenge nonce first, on every connection attempt.
   - **First run:** mkonnect sends a one-time `REGISTRATION_TOKEN` (the challenge is unused on this path). The gateway responds with a freshly generated ML-DSA-65 private key, which mkonnect persists to disk (`KEY_FILE`, `0600` permissions).
   - **Every reconnect after that:** mkonnect proves possession of its private key by signing the challenge (ML-DSA-65, a post-quantum signature scheme). No long-lived secret crosses the wire again.
2. **Serve requests.** The platform sends `http_request` messages down the tunnel, each naming a plugin (e.g. `jenkins`) plus an HTTP method, path, headers, and body. mkonnect resolves the plugin name to an internal base URL (from a local credential entry, else the `PLUGIN_*` registry), optionally injects locally-stored Bearer or Basic credentials, forwards the request to the internal service, and returns the result as an `http_response`. Credentials configured locally (see [Internal tool credentials](#internal-tool-credentials)) are injected on-prem and never transit the tunnel.
3. **Report status.** On connect, on local config change (SIGHUP), and on each heartbeat, mkonnect sends a `connection_status` message listing each plugin's state. The platform can also request an on-demand connectivity probe (`test_connection`), which mkonnect runs against the plugin and answers with a `test_result` — no credential ever appears in either message.
4. **Stay alive.** A heartbeat every 30s — a WebSocket ping (detects silent TCP drops from NAT timeouts or load balancer failures) and an application-level health message (what the gateway actually uses to track connector liveness) — keeps the connection monitored from both sides. If the connection drops, mkonnect reconnects with exponential backoff (1s → 60s cap).

Up to 4 requests are handled concurrently per connector; requests beyond that receive a `429` rather than queuing unbounded.

## Configuration

Runtime configuration comes from environment variables (internal-tool credentials are configured separately via the `connector` CLI — see [Internal tool credentials](#internal-tool-credentials)):

| Variable | Required | Description |
|---|---|---|
| `GATEWAY_URL` | yes | Secure WebSocket URL of the Bridge Gateway, e.g. `wss://<customer-slug>.bridge.memento-platform.com/ws`. Must use `wss://`. |
| `CONNECTOR_ID` | yes | Stable UUID identifying this connector instance. |
| `REGISTRATION_TOKEN` | first run only | One-time token used to register a new connector. Not needed after the private key has been issued and persisted. |
| `KEY_FILE` | no | Path to the ML-DSA-65 private key file. Defaults to `~/.mkonnect/key`. |
| `PROTOCOL_VERSION` | no | Wire protocol version to negotiate. Defaults to `v1`. |
| `CREDS_FILE` | no | Path to the local credential store written by `connector config set`. Defaults to `/data/credentials.json`. |
| `PLUGIN_<NAME>` | no | Base URL of an internal service to expose as plugin `<name>` (lowercased), e.g. `PLUGIN_JENKINS=http://jenkins:8080`. Must be an absolute `http://` or `https://` URL. Use this for endpoints that need no local credential; for endpoints requiring a token, use `connector config set` instead (see below). |
| `ALLOW_INSECURE_GATEWAY` | no | Local development escape hatch. Setting this to `true` permits `ws://` only when `GATEWAY_URL` targets `localhost` or a loopback IP address. Never enable it in production. |

## Internal tool credentials

A plugin's backend can be defined two ways:

- **`PLUGIN_<NAME>` env var** — just a base URL, no authentication injected. Suitable for unauthenticated internal endpoints (e.g. an open Prometheus).
- **Local credential store** — configured on the connector host with the `connector` CLI and persisted to `CREDS_FILE` (`/data/credentials.json`, mode `0600`). It supports Bearer tokens and HTTP Basic credentials (username plus token). **Credentials are injected into the outbound request inside your network and are never sent to the Memento platform.** They are stored in cleartext on the connector's volume (not encrypted at rest); on Kubernetes you can mount a `Secret` as the credential file instead. See [How your credentials are stored](docs/setup.md#how-your-credentials-are-stored).

When both define the same plugin name, the credential store wins.

Plugin base URLs cannot include userinfo, query parameters, or fragments. Requests to plugins with local credentials bypass ambient `HTTP_PROXY` and `HTTPS_PROXY` settings. If an upstream response contains a complete local authorization representation or a supported encoded form of one, mkonnect returns a `502` instead of sending that response through the bridge.

The `connector` subcommand is built into the same binary, so you run it inside the container:

```bash
# Bearer-authenticated tool — tokens must be piped via stdin so they never land
# in the process list or shell history:
printf %s "$JENKINS_TOKEN" | \
  <exec> connector config set jenkins \
    --base-url http://jenkins.internal:8080 --auth bearer --token-stdin

# Basic-authenticated tool — the username and API token stay in the local
# credential store. Only the token is passed through stdin:
printf %s "$BUILD_SERVICE_API_TOKEN" | \
  <exec> connector config set build-service \
    --base-url https://build.internal --auth basic \
    --username "$BUILD_SERVICE_USERNAME" --token-stdin

# Unauthenticated tool:
<exec> connector config set prometheus --base-url http://prometheus.internal:9090

<exec> connector config list      # base URLs; credentials shown masked
<exec> connector config remove jenkins
<exec> connector status           # configured plugins on this connector
```

`<exec>` is `docker exec -i mkonnect` for Docker, or `kubectl exec -i <pod> --` for Kubernetes. The running connector caches credentials in memory, so reload it after a change — `docker kill -s HUP mkonnect` (Docker) or `kubectl rollout restart deployment/mkonnect` (Kubernetes; the distroless image has no shell to signal PID 1). Tokens must be at least 16 characters and are accepted only through `--token-stdin`; `--token` is rejected to prevent exposure through command arguments and shell history. See the [Setup guide](docs/setup.md) for the full walkthrough.

## Releases

Each stable release has a [GitHub Release](https://github.com/memento-knowledge/mkonnect/releases) with release notes, a downloadable Helm chart, and its SHA-256 checksum. Releases use calendar identifiers such as `20260919.0`: the first release on a date is `.0`, then the index increases without gaps. Pin deployments to an explicit release; never use a moving image tag.

The container image uses the calendar release identifier. Helm requires its own SemVer chart version, and each chart's `appVersion` records the calendar release identifier that supplies its default image tag.

The published artifacts are:

- Container image: `public.ecr.aws/h2a8k0r3/memento-connector:<release-id>`
- Helm chart: `oci://public.ecr.aws/h2a8k0r3/mkonnect` at Helm chart version `<chart-version>`

OCI Helm installation is recommended. If you download the chart from a GitHub Release instead, download its `.sha256` asset too and verify it with `sha256sum -c` before installing the local `.tgz` file.

## Running

### Docker

```bash
docker run -d \
  -e GATEWAY_URL=wss://<customer-slug>.bridge.memento-platform.com/ws \
  -e CONNECTOR_ID=<uuid> \
  -e REGISTRATION_TOKEN=<token> \
  -e PLUGIN_JENKINS=http://jenkins:8080 \
  -v mkonnect-data:/data \
  -e KEY_FILE=/data/key \
  public.ecr.aws/h2a8k0r3/memento-connector:<release-id>
```

The container image is a distroless, non-root, statically linked binary — see the [Dockerfile](Dockerfile).

### Kubernetes (Helm)

`REGISTRATION_TOKEN` is only needed for the first-run handshake (see [How it works](#how-it-works)), but it's still a secret — pass it via a values file instead of `--set`/`--set-string`, which would otherwise leak it into your shell history and process list:

```bash
cat > mkonnect-secrets.yaml <<EOF
config:
  registrationToken: "<token>"
EOF

helm install mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --version <chart-version> \
  -f mkonnect-secrets.yaml \
  --set image.repository=public.ecr.aws/h2a8k0r3/memento-connector \
  --set config.gatewayUrl=wss://<customer-slug>.bridge.memento-platform.com/ws \
  --set config.connectorId=<uuid> \
  --set plugins.jenkins=http://jenkins:8080
```

`registrationToken` is written into a Kubernetes `Secret` (`charts/mkonnect/templates/secret.yaml`) alongside `gatewayUrl` and `connectorId`. Keep `mkonnect-secrets.yaml` out of version control and delete it once the connector has completed its first registration — subsequent reconnects use the persisted ML-DSA-65 key instead.

Note that deleting the local file does *not* remove the token from the cluster: it remains in the deployed `Secret` and in Helm's release metadata. Restrict access to both (RBAC on `secrets` and on `helm get values`/release objects in the target namespace), and if the token is no longer needed, rotate or clear it explicitly via `helm upgrade --set config.registrationToken=""` (or a values file) rather than relying on local file deletion alone.

To upgrade, choose the Helm chart version for the desired calendar release. Its default image tag is the chart's `appVersion`:

```bash
helm upgrade mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --version <chart-version> \
  --reuse-values
```

To roll back a failed upgrade, choose the prior Helm revision:

```bash
helm history mkonnect
helm rollback mkonnect <revision>
```

The chart provisions a `PersistentVolumeClaim` so the ML-DSA-65 key survives pod restarts. See [charts/mkonnect/values.yaml](charts/mkonnect/values.yaml) for all options.

### From source

```bash
go build -o mkonnect ./cmd/mkonnect
GATEWAY_URL=wss://... CONNECTOR_ID=... REGISTRATION_TOKEN=... ./mkonnect
```

## Development

```bash
go test ./... -race        # run tests
go vet ./...               # vet
gofmt -l .                 # formatting (should print nothing)
helm lint charts/mkonnect  # lint the chart
```

CI (see [.github/workflows/ci.yml](.github/workflows/ci.yml)) runs tests, a lint gate (`gofmt`, `go vet`, `staticcheck`, `govulncheck`, `actionlint`), and Helm lint on every push and pull request. Versioned artifacts are published only by the [release workflow](.github/workflows/release.yml) when an annotated stable release tag is pushed.

## Project layout

| Path | Purpose |
|---|---|
| [`cmd/mkonnect`](cmd/mkonnect) | Entry point: dispatches the `connector` CLI, loads config, wires up the plugin registry, credential store, and WebSocket client. |
| [`internal/config`](internal/config) | Environment-variable configuration loading and validation. |
| [`internal/auth`](internal/auth) | ML-DSA-65 key storage (`keystore.go`) and challenge signing (`mldsa.go`). |
| [`internal/ws`](internal/ws) | WebSocket client: handshake, reconnect-with-backoff, heartbeat, and the `http_request` / `connection_status` / `test_connection` dispatch loop. |
| [`internal/plugin`](internal/plugin) | Plugin registry (`PLUGIN_*` env vars) and the HTTP reverse-proxy handler that injects local credentials. |
| [`internal/creds`](internal/creds) | Local credential store (`credentials.json`) — atomic, `0600`, reloadable. |
| [`internal/cli`](internal/cli) | The `connector config` / `connector status` subcommands. |
| [`internal/proto`](internal/proto) | JSON message types for the mkonnect ↔ Bridge Gateway protocol. |
| [`internal/version`](internal/version) | Build-time connector version, reported in the handshake. |
| [`charts/mkonnect`](charts/mkonnect) | Helm chart for Kubernetes deployment. |

## License

[MIT](LICENSE)
