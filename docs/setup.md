# Setting up your internal tools

Once the connector is [installed and connected](installation.md), this guide shows how to make
your internal tools (Jenkins, Prometheus, a build service, …) reachable through it, including how
to attach credentials safely.

## How the connector reaches a tool

The platform addresses each tool by a **plugin name** (an opaque label like `jenkins` or
`prometheus`). The connector maps that name to an internal base URL and forwards the request. You
define a plugin in one of two ways:

| Method | Use it for | Authentication |
|---|---|---|
| `PLUGIN_<NAME>` environment variable | Unauthenticated internal endpoints | None injected |
| `connector config set` (local credential store) | Tools that need a token or username/password | Injected locally, on-prem |

If the same plugin name is defined both ways, the local credential store wins.

**The trust boundary.** Credentials you configure with `connector config set` are stored only on
the connector's local volume (`/data/credentials.json`, owner-only) and are injected into the
outbound request *inside your network*. They are never sent to the Memento platform. The connector
additionally strips credential-bearing response headers and refuses to return a response that
reflects your local token back through the tunnel.

## Running the connector CLI

The `connector` subcommand is built into the same binary, so run it inside the running container:

- **Docker:** `docker exec -i mkonnect connector ...`
- **Kubernetes:** `kubectl exec -i <pod> -- connector ...`

The examples below use `<exec>` to stand in for whichever applies. The `-i` (interactive) flag
matters: tokens are read from **stdin**, never from a command-line flag.

## Configure an unauthenticated tool

For an open endpoint like a Prometheus with no auth, either set a `PLUGIN_<NAME>` variable at
install time, or register it with the CLI (base URL only):

```bash
<exec> connector config set prometheus --base-url http://prometheus.internal:9090
```

## Configure a bearer-token tool

For a tool that authenticates with a bearer token (a common Jenkins setup), pipe the token through
stdin so it never appears in the process list or your shell history:

```bash
printf %s "$JENKINS_TOKEN" | \
  <exec> connector config set jenkins \
    --base-url http://jenkins.internal:8080 \
    --auth bearer --token-stdin
```

The connector will add `Authorization: Bearer <token>` to each request it forwards to `jenkins`.

## Configure a basic-auth tool

For a tool using HTTP Basic authentication, provide a username and pipe the password/token through
stdin:

```bash
printf %s "$BUILD_SERVICE_PASSWORD" | \
  <exec> connector config set build-service \
    --base-url https://build.internal \
    --auth basic --username "$BUILD_SERVICE_USERNAME" --token-stdin
```

The connector adds the `Authorization: Basic …` header, encoding `username:token` locally.

## Token rules

- **Stdin only.** Tokens must be supplied through `--token-stdin`. A `--token <value>` flag is
  rejected with an error, so a secret can't leak into shell history or `ps` output.
- **Minimum length.** Tokens must be at least 16 characters.
- **Trailing newline is trimmed**, so `printf %s` and `echo` both work; prefer `printf %s` to avoid
  a stray newline in the first place.
- A username (basic auth) must not contain a `:`.

## Manage configured tools

```bash
<exec> connector config list      # base URLs; credentials shown masked
<exec> connector status           # this connector's configured plugins
<exec> connector config remove jenkins
```

Credentials are never printed back — `list` shows the auth mode and masks the secret.

### Applying changes

The `connector config` commands run as a short-lived process and write the change to disk. The
long-running connector keeps its credentials in memory and does **not** pick up the change on its
own — you must tell it to reload:

- **Docker** — send the connector a `SIGHUP` for a live reload (no downtime):

  ```bash
  docker kill -s HUP mkonnect
  ```

- **Kubernetes** — the connector image is distroless (no shell), so restart the workload instead;
  the private key persists on the volume, so it reconnects immediately:

  ```bash
  kubectl rollout restart deployment/mkonnect
  ```

Either way the connector then pushes a fresh status report to the portal. Restarting the Docker
container (`docker restart mkonnect`) also works and is equivalent to a reload.

## Verify a tool from the portal

After configuring a tool, open **Integrations → On-Prem Connector** (or the tool's integration
card) in the Memento portal:

- The connector reports each plugin's state — **configured & connected**, **configured (error)**,
  or **not configured** — on connect, whenever the local config changes, and on each heartbeat.
- Use **Test connection** to trigger an on-demand connectivity probe. The connector runs a request
  against the tool's base URL and reports the result: `connected`, `auth_failure`, `unreachable`,
  or `timeout`. No credential is ever included in the test request or its result.

## Troubleshooting

| Result | Meaning and fix |
|---|---|
| `not configured` | No `PLUGIN_<NAME>` variable and no credential entry for this plugin name. Add one with `connector config set`, and confirm the name matches what the platform expects. |
| `auth_failure` (401/403) | The tool rejected the credential. Re-check the token/username and re-run `connector config set`. For bearer tools, confirm the token has the needed scopes. |
| `unreachable` | The connector couldn't open a connection to the base URL. Confirm the URL and port are correct and reachable from the connector's network location, and that DNS resolves. Diagnostics are intentionally coarse (no internal addresses are sent to the platform). |
| `timeout` | The tool accepted the connection but didn't respond in time. Check the tool's health and any intermediate proxy. |
| Credential change didn't take effect | The running connector caches credentials in memory. Reload it after a change: `docker kill -s HUP mkonnect` (Docker) or `kubectl rollout restart deployment/mkonnect` (Kubernetes). See [Applying changes](#applying-changes). |
| A credentialed tool is unreachable but a proxy is set | Requests that carry a **local credential** deliberately bypass ambient `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`, so the token goes straight to the tool rather than to a proxy. Ensure the connector has *direct* network access to the tool's host. (Unauthenticated `PLUGIN_<NAME>` requests still honor the ambient proxy settings.) |

For connector-level connection problems (the bridge itself showing degraded), see the
[Installation guide's troubleshooting](installation.md#troubleshooting).
