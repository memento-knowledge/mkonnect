# mkonnect

[![CI](https://github.com/memento-knowledge/mkonnect/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/memento-knowledge/mkonnect/actions/workflows/ci.yml)

Lightweight on-prem connector for the Memento Knowledge (mk) platform. It runs inside a customer's network and bridges internal tools — Jenkins, Prometheus, Coralogix, and more — to the Memento platform over a secure WebSocket tunnel, so nothing needs to be exposed to the public internet.

## How it works

```
Memento platform ── wss:// ──▶ Bridge Gateway ── WebSocket ──▶ mkonnect ── HTTP ──▶ Jenkins / Prometheus / ...
```

1. **Connect.** mkonnect dials the Bridge Gateway over `wss://`. The gateway sends a random challenge nonce first, on every connection attempt.
   - **First run:** mkonnect sends a one-time `REGISTRATION_TOKEN` (the challenge is unused on this path). The gateway responds with a freshly generated ML-DSA-65 private key, which mkonnect persists to disk (`KEY_FILE`, `0600` permissions).
   - **Every reconnect after that:** mkonnect proves possession of its private key by signing the challenge (ML-DSA-65, a post-quantum signature scheme). No long-lived secret crosses the wire again.
2. **Serve requests.** The platform sends `data` messages down the tunnel addressed to a named plugin (e.g. `jenkins`) with an HTTP method, path, and body. mkonnect resolves the plugin name against the `PLUGIN_*` registry, reverse-proxies the request to the corresponding internal service, and streams the response back over the same connection.
3. **Stay alive.** A heartbeat every 30s — a WebSocket ping (detects silent TCP drops from NAT timeouts or load balancer failures) and an application-level health message (what the gateway actually uses to track connector liveness) — keeps the connection monitored from both sides. If the connection drops, mkonnect reconnects with exponential backoff (1s → 60s cap).

Up to 4 requests are handled concurrently per connector; requests beyond that receive a `429` rather than queuing unbounded.

## Configuration

mkonnect is configured entirely through environment variables:

| Variable | Required | Description |
|---|---|---|
| `GATEWAY_URL` | yes | WebSocket URL of the Bridge Gateway, e.g. `wss://<customer-slug>.bridge.memento-platform.com/ws`. Must use `wss://` in production — `ws://` works but logs a warning, since private key material can be transmitted during first-run registration. |
| `CONNECTOR_ID` | yes | Stable UUID identifying this connector instance. |
| `REGISTRATION_TOKEN` | first run only | One-time token used to register a new connector. Not needed after the private key has been issued and persisted. |
| `KEY_FILE` | no | Path to the ML-DSA-65 private key file. Defaults to `~/.mkonnect/key`. |
| `PROTOCOL_VERSION` | no | Wire protocol version to negotiate. Defaults to `v1`. |
| `PLUGIN_<NAME>` | no | Base URL of an internal service to expose as plugin `<name>` (lowercased), e.g. `PLUGIN_JENKINS=http://jenkins:8080`. Must be an absolute `http://` or `https://` URL. Add one per tool you want to bridge. |

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
  public.ecr.aws/<alias>/memento-connector:latest
```

The container image is a distroless, non-root, statically linked binary — see the [Dockerfile](Dockerfile).

### Kubernetes (Helm)

`REGISTRATION_TOKEN` is only needed for the first-run handshake (see [How it works](#how-it-works)), but it's still a secret — pass it via a values file instead of `--set`/`--set-string`, which would otherwise leak it into your shell history and process list:

```bash
cat > mkonnect-secrets.yaml <<EOF
config:
  registrationToken: "<token>"
EOF

helm install mkonnect charts/mkonnect \
  -f mkonnect-secrets.yaml \
  --set image.repository=public.ecr.aws/<alias>/memento-connector \
  --set config.gatewayUrl=wss://<customer-slug>.bridge.memento-platform.com/ws \
  --set config.connectorId=<uuid> \
  --set plugins.jenkins=http://jenkins:8080
```

`registrationToken` is written into a Kubernetes `Secret` (`charts/mkonnect/templates/secret.yaml`) alongside `gatewayUrl` and `connectorId`. Keep `mkonnect-secrets.yaml` out of version control and delete it once the connector has completed its first registration — subsequent reconnects use the persisted ML-DSA-65 key instead.

Note that deleting the local file does *not* remove the token from the cluster: it remains in the deployed `Secret` and in Helm's release metadata. Restrict access to both (RBAC on `secrets` and on `helm get values`/release objects in the target namespace), and if the token is no longer needed, rotate or clear it explicitly via `helm upgrade --set config.registrationToken=""` (or a values file) rather than relying on local file deletion alone.

The chart provisions a `PersistentVolumeClaim` so the ML-DSA-65 key survives pod restarts. See [charts/mkonnect/values.yaml](charts/mkonnect/values.yaml) for all options.

### From source

```bash
go build -o mkonnect ./cmd/mkonnect
GATEWAY_URL=wss://... CONNECTOR_ID=... REGISTRATION_TOKEN=... ./mkonnect
```

## Development

```bash
go test ./... -v -race   # run tests
helm lint charts/mkonnect  # lint the chart
```

CI (see [.github/workflows/ci.yml](.github/workflows/ci.yml)) runs tests and Helm lint on every push and pull request, and builds/pushes the Docker image to Amazon ECR Public on merges to `main`.

## Project layout

| Path | Purpose |
|---|---|
| [`cmd/mkonnect`](cmd/mkonnect) | Entry point: loads config, wires up the plugin registry and WebSocket client. |
| [`internal/config`](internal/config) | Environment-variable configuration loading and validation. |
| [`internal/auth`](internal/auth) | ML-DSA-65 key storage (`keystore.go`) and challenge signing (`mldsa.go`). |
| [`internal/ws`](internal/ws) | WebSocket client: handshake, reconnect-with-backoff, keepalive, and the request dispatch loop. |
| [`internal/plugin`](internal/plugin) | Plugin registry (`PLUGIN_*` env vars) and the HTTP reverse-proxy handler. |
| [`internal/proto`](internal/proto) | JSON message types for the mkonnect ↔ Bridge Gateway protocol. |
| [`charts/mkonnect`](charts/mkonnect) | Helm chart for Kubernetes deployment. |

## License

[MIT](LICENSE)
