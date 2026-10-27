# mkonnect

[![CI](https://github.com/memento-knowledge/mkonnect/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/memento-knowledge/mkonnect/actions/workflows/ci.yml)

Lightweight on-prem connector for the Memento Knowledge (mk) platform. It runs inside a customer's network and bridges internal tools — Jenkins, Prometheus, Coralogix, and more — to the Memento platform over a secure WebSocket tunnel, so nothing needs to be exposed to the public internet.

## Documentation

Administrator guides live in [`docs/`](docs/):

- **[Installation guide](docs/installation.md)** — [system requirements](docs/installation.md#system-requirements), deploy with Docker or Helm, verify the connection, upgrade, and uninstall.
- **[Setup guide](docs/setup.md)** — make your internal tools reachable and attach credentials safely.

The rest of this README is a conceptual overview and configuration reference.

## How it works

```
Memento platform ── wss:// ──▶ Bridge Gateway ── WebSocket ──▶ mkonnect ── HTTP ──▶ Jenkins / Prometheus / ...
```

1. **Connect.** mkonnect dials the Bridge Gateway over `wss://`. The gateway sends a random challenge nonce first, on every connection attempt.
   - **First run:** mkonnect sends a one-time `REGISTRATION_TOKEN` (the challenge is unused on this path). The gateway responds with a freshly generated ML-DSA-65 private key, which mkonnect persists to disk (`KEY_FILE`, `0600` permissions).
   - **Every reconnect after that:** mkonnect proves possession of its private key by signing the challenge (ML-DSA-65, a post-quantum signature scheme). No long-lived secret crosses the wire again.
2. **Serve requests.** The platform sends `http_request` messages down the tunnel, each naming a plugin (e.g. `jenkins`) plus an HTTP method, path (see [Request paths](#request-paths)), headers, body, and an optional `expires_at` deadline (see [Connector error markers](#connector-error-markers)). mkonnect resolves the plugin name to an internal base URL (from a local credential entry, else the `PLUGIN_*` registry), optionally injects locally-stored Bearer or Basic credentials, forwards the request to the internal service, and returns the result as an `http_response`. Credentials configured locally (see [Internal tool credentials](#internal-tool-credentials)) are injected on-prem and never transit the tunnel.
3. **Report status.** On connect, on local config change (SIGHUP), and on each heartbeat, mkonnect sends a `connection_status` message listing each plugin's state. The platform can also request an on-demand connectivity probe (`test_connection`), which mkonnect runs against the plugin and answers with a `test_result` — no credential ever appears in either message.
4. **Stay alive.** A heartbeat every 30s — a WebSocket ping (detects silent TCP drops from NAT timeouts or load balancer failures) and an application-level health message (what the gateway actually uses to track connector liveness) — keeps the connection monitored from both sides. If the connection drops, mkonnect reconnects with exponential backoff (1s → 60s cap).

### Limits

Each forwarded request is subject to fixed resource limits. They bound the connector's memory and stop one slow or oversized request from starving others; the platform sizes its own retries and timeouts against them. Every over-limit outcome is a **visible, marked reply** (see [Connector error markers](#connector-error-markers)) — never a silently dropped connection — except the concurrency `429`, which is unmarked.

| Limit | Value | When exceeded |
|---|---|---|
| Concurrent requests | 4 | A `429` (unmarked); the request is refused rather than queued unbounded. |
| Request body | 32 MiB | `413` with `X-Mkonnect-Error: request_too_large`; rejected before being forwarded. |
| Response body | 32 MiB | `413` with `X-Mkonnect-Error: response_too_large`; the upstream was contacted and may have acted, but the oversized response is withheld. |
| Upstream timeout | 10s (default) | `504` with `X-Mkonnect-Error: upstream_timeout` if the upstream does not respond in time (connecting, sending, or reading the response). |

The 4-request concurrency limit and the 32 MiB body caps together keep the connector's peak buffering bounded — on the order of its default 128 MiB container memory limit, though a single request buffers its request body, the response, and a copy of it, so the true peak is a small multiple of 32 MiB per in-flight request. The concurrency and body limits are therefore fixed.

The **upstream timeout is configurable per plugin** (the other limits are not). Set `--upstream-timeout` on `connector config set` — or the `upstream_timeout` field in the credentials file — to a Go duration with a unit between `1s` and `10m` (e.g. `30s`, `2m`); omit it to keep the 10s default. Raise it for upstreams that are legitimately slow, such as a large commit or a slow self-managed instance. It applies only to forwarded `http_request`s, not to the `test_connection` probe (which keeps its own fixed timeout); it bounds the whole request, but the connect phase (30s) and TLS handshake (10s) keep their own fixed ceilings, so a larger value does not extend them. The platform stops waiting at its own caller deadline (see `expires_at` below), so raising this timeout does not make the platform wait longer — the connector checks `expires_at` only on arrival and does not clamp an in-flight request to it, so a long value holds a worker slot for its full duration, and a request still running when the connector drains on shutdown (a 10s window) may not return a reply. A change takes effect after a reload (SIGHUP or restart). See [Tuning the upstream timeout](docs/setup.md#tuning-the-upstream-timeout) for examples.

### Connector error markers

Some `http_response` replies are **synthesised by the connector while forwarding a request** (for example when a request is rejected before it is forwarded, or when the upstream could not be reached) rather than relayed from the upstream service. Because a synthesised status such as `504` is otherwise indistinguishable from the same status returned by the upstream, mkonnect tags every such response with a response header:

```
X-Mkonnect-Error: <code>
```

Responses relayed from the upstream never carry this header; any `X-Mkonnect-Error` the upstream itself sets is stripped before relaying, so the marker is always trustworthy. Match the header name case-insensitively. The header is optional and additive — older platforms that do not read it are unaffected.

The code tells the consumer whether the request can have reached the upstream:

| Code | Status | Request reached upstream? | Meaning |
|---|---|---|---|
| `provider_not_found` | 404 | No | No backend is configured for the requested provider. |
| `method_not_allowed` | 405 | No | The HTTP method is not permitted. |
| `invalid_path` | 400 | No | The request path was malformed or did not resolve to the configured host. |
| `path_blocked` | 400 | No | The path was rejected by path-confinement checks (e.g. traversal outside the base path). |
| `invalid_base_url` | 500 | No | The configured base URL for the provider is invalid (misconfiguration). |
| `request_too_large` | 413 | No | The request body exceeded the size limit. |
| `upstream_unreachable` | 504 | No | The connection could not be established (dial failure). |
| `response_too_large` | 413 | Yes (response withheld) | The upstream response exceeded the size limit. The upstream may already have acted. |
| `upstream_credential_leak` | 502 | Yes (response withheld) | The upstream response reflected locally-held credentials and was withheld to keep them on-prem. |
| `upstream_timeout` | 504 | Unknown | The upstream did not respond in time (connecting, sending, or reading the response). |
| `upstream_error` | 504 | Unknown | A transport error other than a timeout or dial failure (e.g. TLS failure or reset); the request may already have been sent. |
| `expired` | 504 | No | The request's `expires_at` deadline had already passed when it was received; it was not forwarded. |

A request may carry an optional `expires_at` deadline (RFC 3339 with a UTC offset, e.g. `2026-10-20T12:00:00Z`; fractional seconds allowed). When present and already in the past on arrival, the connector does not forward the request and returns the `expired` marker above — a safe default against the outbound queue redelivering a request the caller abandoned after its timeout. The deadline is only checked on arrival; it does not bound the upstream call itself. It is compared against the connector host's clock, which should be NTP-synced. The field is optional and backward-compatible: an absent value (older platform messages) or an unparseable string is ignored and the request is forwarded as before. It must be a JSON *string*: like any other field, a non-string value (e.g. a number) fails to decode the message, which is then dropped with no response.

The status code on a synthesised response is kept for backward compatibility and is not always a precise fit (e.g. `upstream_error` reuses `504`); the `X-Mkonnect-Error` code is the authoritative signal.

Connector responses produced outside the request-forwarding path — the backpressure `429` and lifecycle responses such as the suspension `503` — are not marked yet.

### Request paths

The request `path` is forwarded with its percent-encoding **preserved**, so the request-target the upstream receives is the one the platform sent — provided it round-trips cleanly (see the fallback below). A single `%2F` reaches the upstream as `%2F`, not as a decoded `/`.

- **Encode each segment once.** An escaped `%25` is itself preserved, so double-encoding (`%252F`) would arrive literally as `%252F`, not `%2F`. Percent-encode each path segment exactly once. This assumes the upstream decodes the path once, as HTTP servers normally do; an upstream that decodes twice is outside this contract.
- **Base-URL path prefix.** A non-default path prefix on the plugin's base URL is prepended with its own encoding preserved — `/api/v1` + `/group%2Fsub` → `/api/v1/group%2Fsub` (a trailing slash on the base is not doubled).
- **Query string** is passed through as received.
- **Plain paths are unchanged**, and lower-case escapes such as `%2f` are kept as written.

Security and confinement checks run on the **decoded, normalized** path, not the escaped form:

- A path that is malformed, does not begin with `/`, does not resolve to the configured host, or carries userinfo or an authority (`//host/…`, `//user:pw@host/…`) is rejected with [`invalid_path`](#connector-error-markers) and never forwarded. An encoded `//` is not treated as an authority, so it cannot redirect the request to another host — `/%2F%2Fexample` is handled as the path `/example` on the configured upstream.
- A `..` traversal that escapes a non-root base-URL path prefix is rejected with [`path_blocked`](#connector-error-markers). With a root base URL there is no prefix to escape, so `..` segments are simply normalized away rather than rejected.

**Fallback.** If the escaped path is not an unambiguous round-trip — for example it mixes a raw reserved byte with escapes (`/a%2Fb c`), or contains `.`, `..`, or `//` segments — the connector forwards the **decoded, cleaned** path instead. In that case an escaped slash does not survive and segment boundaries change (`/a%2Fb c` → `/a/b%20c`). To keep `%2F` intact, send fully-encoded paths with no raw reserved bytes and no `.`, `..`, or `//` segments.

## Configuration

Runtime configuration comes from environment variables (internal-tool credentials are configured separately via the `connector` CLI — see [Internal tool credentials](#internal-tool-credentials)):

| Variable | Required | Description |
|---|---|---|
| `GATEWAY_URL` | yes | Secure WebSocket URL of the Bridge Gateway. Copy it exactly from the Memento portal; it must use `wss://`. |
| `CONNECTOR_ID` | yes | Stable UUID identifying this connector instance. |
| `REGISTRATION_TOKEN` | first run only | One-time token used to register a new connector. Not needed after the private key has been issued and persisted. |
| `KEY_FILE` | no | Path to the ML-DSA-65 private key file. Defaults to `~/.mkonnect/key`. |
| `PROTOCOL_VERSION` | no | Wire protocol version to negotiate. Defaults to `v1`. |
| `CREDS_FILE` | no | Path to the local credential store written by `connector config set`. Defaults to `/data/credentials.json`. |
| `PLUGIN_<NAME>` | no | Base URL of an internal service to expose as plugin `<name>` (lowercased), e.g. `PLUGIN_JENKINS=http://jenkins.internal:8080`. Must be an absolute `http://` or `https://` URL. Use this for endpoints that need no local credential; for endpoints requiring a token, use `connector config set` instead (see below). |
| `ALLOW_INSECURE_GATEWAY` | no | Local development escape hatch. Setting this to `true` permits `ws://` only when `GATEWAY_URL` targets `localhost` or a loopback IP address. Never enable it in production. |

## Internal tool credentials

A plugin's backend can be defined two ways:

- **`PLUGIN_<NAME>` env var** — just a base URL, no authentication injected. Suitable for unauthenticated internal endpoints (e.g. an open Prometheus).
- **Local credential store** — configured on the connector host with the `connector` CLI and persisted to `CREDS_FILE` (`/data/credentials.json`, mode `0600`). It supports Bearer tokens and HTTP Basic credentials (username plus token). **Credentials are injected into the outbound request inside your network and are never sent to the Memento platform.** When a local credential is applied, caller-supplied credential headers (`Authorization`, `Cookie`, `PRIVATE-TOKEN`, `JOB-TOKEN`, `X-Api-Key`, `X-Auth-Token`, `Proxy-Authorization`) are stripped first, so the local credential is the only one the tool sees; add more with `--strip-headers`. They are stored in cleartext on the connector's volume (not encrypted at rest); on Kubernetes you can mount a `Secret` as the credential file instead. See [How your credentials are stored](docs/setup.md#how-your-credentials-are-stored).

When both define the same plugin name, the credential store wins.

Plugin base URLs cannot include userinfo, query parameters, or fragments. Requests to plugins with local credentials bypass ambient `HTTP_PROXY` and `HTTPS_PROXY` settings. If an upstream response contains a complete local authorization representation or a supported encoded form of one, mkonnect returns a `502` instead of sending that response through the bridge.

The `connector` command is a subcommand of the `/mkonnect` binary, so you run it inside the container by the binary's path (`docker exec`/`kubectl exec` bypass the image entrypoint):

```bash
# Bearer-authenticated tool (e.g. Grafana) — tokens must be piped via stdin so
# they never land in the process list or shell history:
printf %s "$GRAFANA_TOKEN" | \
  <exec> connector config set grafana \
    --base-url http://grafana.internal:3000 --auth bearer --token-stdin

# Basic-authenticated tool — e.g. Jenkins, whose REST API uses HTTP Basic with the
# API token as the password (it does not accept Bearer). The username and token
# stay in the local credential store; only the token is passed through stdin:
printf %s "$JENKINS_API_TOKEN" | \
  <exec> connector config set jenkins \
    --base-url http://jenkins.internal:8080 --auth basic \
    --username "$JENKINS_USERNAME" --token-stdin

# Unauthenticated tool:
<exec> connector config set prometheus --base-url http://prometheus.internal:9090

<exec> connector config list      # base URLs; credentials shown masked
<exec> connector config remove jenkins
<exec> connector status           # configured plugins on this connector
```

`<exec>` is `docker exec -i mkonnect /mkonnect` for Docker, or `kubectl exec -i <pod> -- /mkonnect` for Kubernetes (replace `mkonnect`/`<pod>` with your container name or pod). Invoking a bare `connector` fails with `executable file not found in $PATH`. The running connector caches credentials in memory, so reload it after a change — `docker kill -s HUP mkonnect` (Docker) or `kubectl rollout restart deployment/mkonnect` (Kubernetes; the distroless image has no shell to signal PID 1). Tokens must be at least 16 characters and are accepted only through `--token-stdin`; `--token` is rejected to prevent exposure through command arguments and shell history. See the [Setup guide](docs/setup.md) for the full walkthrough.

## Releases

Each stable release has a [GitHub Release](https://github.com/memento-knowledge/mkonnect/releases) with release notes, a downloadable Helm chart, and its SHA-256 checksum. Releases use calendar identifiers in the form `YYYYMMDD.n`: the first release on a date is `.0`, then the index increases without gaps. Pin deployments to an explicit release; never use a moving image tag.

The container image uses the calendar release identifier. Helm requires its own SemVer chart version, and each chart's `appVersion` records the calendar release identifier that supplies its default image tag.

The published artifacts are:

- Container image: `public.ecr.aws/h2a8k0r3/memento-connector:<release-id>`
- Helm chart: `oci://public.ecr.aws/h2a8k0r3/mkonnect` at Helm chart version `<chart-version>`

OCI Helm installation is recommended. If you download the chart from a GitHub Release instead, download its `.sha256` asset too and verify it with `sha256sum -c` before installing the local `.tgz` file.

## Running

### Docker

Follow the [Docker installation guide](docs/installation.md#option-a--docker). It keeps the
one-time registration token out of shell history and source checkouts, then removes it from the
container configuration only after the portal confirms registration. The guide also covers Docker
Desktop, Linux host, and sibling-container networking.

The container image is a distroless, non-root, statically linked binary — see the [Dockerfile](Dockerfile).

### Kubernetes (Helm)

Follow the [Helm installation guide](docs/installation.md#option-b--kubernetes-helm). It passes the
token using `--set-file`, retains it until the portal confirms registration, and explains the
remaining Helm release-history exposure. The chart provisions a `PersistentVolumeClaim` so the
ML-DSA-65 key survives pod restarts. See [charts/mkonnect/values.yaml](charts/mkonnect/values.yaml)
for all options.

### From source

```bash
# Bash or Zsh; this path is intended for local development.
go build -o mkonnect ./cmd/mkonnect

umask 077
mkonnect_config_dir="${XDG_CONFIG_HOME:-$HOME/.config}/mkonnect"
mkdir -p "$mkonnect_config_dir"
chmod 700 "$mkonnect_config_dir"
: > "$mkonnect_config_dir/registration.token"
chmod 600 "$mkonnect_config_dir/registration.token"
printf 'Paste registration token: ' >&2
IFS= read -r -s REGISTRATION_TOKEN
printf '\n' >&2
printf %s "$REGISTRATION_TOKEN" > "$mkonnect_config_dir/registration.token"
unset REGISTRATION_TOKEN

REGISTRATION_TOKEN="$(<"$mkonnect_config_dir/registration.token")"
export REGISTRATION_TOKEN
GATEWAY_URL=<gateway-url-from-portal> CONNECTOR_ID=<connector-id-from-portal> ./mkonnect
```

After it has registered, stop the process, then restart without the token:

```bash
unset REGISTRATION_TOKEN
rm "$mkonnect_config_dir/registration.token"
GATEWAY_URL=<gateway-url-from-portal> CONNECTOR_ID=<connector-id-from-portal> ./mkonnect
```

For production, use the Docker or Helm instructions above.

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
