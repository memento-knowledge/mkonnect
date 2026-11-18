# Setting up your internal tools

Once the connector is [installed and connected](installation.md), this guide shows how to make
your internal tools (Jenkins, Prometheus, a build service, …) reachable through it, including how
to attach credentials safely.

## Set your values once

The Kubernetes commands in this guide read the namespace of your connector's release from the
shell variable `NAMESPACE`, so you can copy them as written. Set it in the Bash or Zsh session you
run them from, to the namespace you used when you [installed the
connector](installation.md#use-one-namespace-for-every-command). It lasts only for the current
session, so set it again in a new one: a `helm` or `kubectl` command with an empty namespace
silently uses your current context's namespace.

(If you use Zsh, run `setopt interactivecomments` once first, so that the `#` comments in the
commands are ignored as they are in Bash.)

```bash
# Kubernetes only: the namespace of the connector's release.
NAMESPACE=mkonnect
```

The commands that change a Helm value also use `CHART_VERSION`, the chart version you run, so
that they do not move the release to a newer chart as a side effect; see
[Choosing a release](installation.md#choosing-a-release) for how to set it. The connector CLI
commands use a small `mkonnect_exec` function, defined under
[Running the connector CLI](#running-the-connector-cli).

## How the connector reaches a tool

The platform addresses each tool by a **plugin name** (an opaque label like `jenkins` or
`prometheus`). The connector maps that name to an internal base URL and forwards the request. You
define a plugin in one of these ways:

| Method | Use it for | Authentication |
|---|---|---|
| `PLUGIN_<NAME>` environment variable (on Kubernetes, the chart's `plugins:` values) | Unauthenticated internal endpoints | None injected |
| `connector config set` (local credential store) | Tools that need a token or username/password | Injected locally, on-prem |
| A credentials file mounted from a Kubernetes `Secret` | Tools that need a token or username/password, when `exec` is not available | Injected locally, on-prem |

If the same plugin name is defined both ways, the local credential store wins. To configure a tool
on a cluster that does not allow `kubectl exec`, see
[Configuring tools without `kubectl exec`](#configuring-tools-without-kubectl-exec).

**The tool names in this guide are examples.** `jenkins`, `grafana`, and `prometheus` are
illustrative plugin names standing in for your own tools — mkonnect is tool-agnostic and forwards to
any HTTP(S) service. Choose the authentication mode below by **how your tool authenticates** (a
bearer token, HTTP Basic, or none), not by which product it is, and use the plugin name the platform
expects for that integration.

**The trust boundary.** Credentials you configure with `connector config set` are stored only on
the connector's local volume (`/data/credentials.json`, owner-only) and are injected into the
outbound request *inside your network*. They are never sent to the Memento platform. The connector
additionally strips credential-bearing response headers and refuses to return a response that
reflects your local token back through the tunnel.

**Caller credential headers are stripped.** When a plugin has a local credential, the connector
drops any credential-bearing header on the inbound request before injecting its own — the defaults
are `Authorization`, `Proxy-Authorization`, `Cookie`, `PRIVATE-TOKEN`, `JOB-TOKEN`, `X-Api-Key` and
`X-Auth-Token` — so the locally configured credential is the only one the tool ever sees, even if a
request arrives carrying another. Add tool-specific headers to the list with `--strip-headers`
(comma-separated) on `connector config set`, or the `strip_headers` field in the credentials file;
the defaults are always enforced and cannot be removed.

**Caller credential query parameters are stripped too.** Some tools accept a credential in the URL
query string as well as a header — a REST API that honours `?private_token=`, `?job_token=` or
`?access_token=`, sometimes in preference to the `Authorization` header. So when a plugin has a local
credential, the connector also drops credential-bearing query parameters on the inbound request
before forwarding — the defaults are `private_token`, `job_token`, `access_token` and `token` — so
the locally configured credential is the only one the tool honours, in the query string as well as
the headers. Add tool-specific parameter names (for example a trigger token) with
`--strip-query-params` (comma-separated) on `connector config set`, or the `strip_query_params` field
in the credentials file; the defaults are always enforced and cannot be removed. Matching folds case
and ignores any `name[...]` array/operator suffix, so a casing or array-syntax variant cannot slip a
credential through. Note two boundaries: a credential in the **request body** (a form or JSON field)
is forwarded unchanged — bodies are the tool's own boundary — and a parameter that selects a
different principal rather than authenticating (for example GitLab's `?sudo=` with an admin token) is
not a credential and is not stripped; add it to `strip_query_params` if your tool honours it. A
documented consequence: a call that deliberately relies on a URL credential, such as GitLab's
`POST /projects/:id/trigger/pipeline?token=…`, no longer works through a plugin that has its own
local credential — that is the intended guarantee, not a regression.

## Configuring tools without `kubectl exec`

Some Kubernetes clusters do not allow `pods/exec`, which rules out running `connector config set`
inside the pod (see [Running the connector CLI](#running-the-connector-cli)). You can still
configure every kind of tool without it. There are three ways to configure a tool:

| Path | Needs `exec`? | Use it for | Where the configuration lives |
|---|---|---|---|
| `connector config set` | Yes (`docker exec` / `kubectl exec`) | Any tool, credentialed or not | `/data/credentials.json` on the connector's volume |
| Mounted Secret (`credentials.secretName`) | No | Tools that need a credential; also anonymous tools and per-tool options | A `Secret` you create in the release's namespace, mounted read-only |
| `plugins:` Helm values | No | Anonymous tools: a base URL and nothing else | The Helm release, as `PLUGIN_<NAME>` environment variables |

Neither of the two paths that avoid `exec` sends a credential through the Memento platform: the
Secret is one you create in your own cluster, so credentials stay local, as described in
[the trust boundary](#how-the-connector-reaches-a-tool).

**What "anonymous" means.** A base-URL-only tool is one for which the connector adds no credential
of its own: it injects no `Authorization` header and does not apply the default credential-header
and query-parameter stripping described above, which applies only when a local credential is
applied. Headers that arrive with a request are passed through (except hop-by-hop headers, and any
names you list in the entry's `strip_headers`), query parameters are passed through (except any
names you list in `strip_query_params`), and the connector still honours the ambient
`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` settings. An entry in the credentials file with no `auth` is
valid and is an anonymous tool, and so is a `plugins:` value.

### A credentialed tool: the mounted Secret

Create the Secret and point the chart at it as described in
[Mounting a Secret as the credential file](#mounting-a-secret-as-the-credential-file-kubernetes);
no `exec` is involved. The file can hold credentialed and anonymous entries side by side:

```json
{
  "jenkins": {
    "base_url": "http://jenkins.internal:8080",
    "auth": "basic",
    "username": "REPLACE_WITH_USERNAME",
    "token": "REPLACE_WITH_API_TOKEN"
  },
  "prometheus": {
    "base_url": "http://prometheus.internal:9090"
  }
}
```

The Secret must exist in the release's namespace before the pod starts: the volume is not
optional, so a missing Secret leaves the pod in `ContainerCreating`, which has no logs;
`kubectl describe pods --namespace "$NAMESPACE" --selector app.kubernetes.io/name=mkonnect` shows
the reason. A file that is not valid
JSON, or that holds an entry with an invalid credential or option, stops the connector at startup
with a `creds store load failed` error and the pod restarts repeatedly; read
`kubectl logs --previous deployment/mkonnect --namespace "$NAMESPACE"` to see which entry is
rejected. A malformed `base_url` is not checked at startup: requests and **Test connection** for
that tool fail with an `invalid base URL` error instead.

### An anonymous tool: the `plugins:` Helm values

Each entry under `plugins:` in the chart values is rendered as a `PLUGIN_<NAME>` environment
variable on the connector's Deployment: `plugins.jenkins` becomes `PLUGIN_JENKINS`, and the plugin
name is its lower-case form, `jenkins`. Nothing else is needed: no Secret, no `exec`, and no
credential.

```yaml
plugins:
  jenkins: http://jenkins.internal:8080
```

Put the same setting in the install command, or add it to a running release. This command only
changes a value, so it asks for the chart version you run (`CHART_VERSION`) and keeps the release
on it; without `--version`, Helm would also move the release to the newest chart. If the release
has [git-sync](git-sync.md) enabled, use `--reset-then-reuse-values` instead of `--reuse-values`
(see the [git-sync Helm notes](git-sync.md#option-b-kubernetes-helm)):

```bash
helm upgrade mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --namespace "$NAMESPACE" --version "${CHART_VERSION:?set CHART_VERSION to the chart version you run}" \
  --reuse-values \
  --set plugins.jenkins=http://jenkins.internal:8080
```

The value must be an absolute `http` or `https` URL with no userinfo, query parameters or
fragment. The connector validates every `PLUGIN_*` value at startup and exits with a
`plugin registry load failed` error if one is malformed, so if the pod restarts repeatedly after a
change, read `kubectl logs --previous deployment/mkonnect --namespace "$NAMESPACE"`.

### If a name is defined in more than one place

The credential entry wins. When a credentials-file entry for a plugin name sets a `base_url`, the
connector uses that URL and the entry's own credential and options, and ignores the
`PLUGIN_<NAME>` value for the same name. The `PLUGIN_<NAME>` URL is used when there is no entry for
that name. (`connector config set` always requires a base URL. A hand-written file entry that
leaves `base_url` out is accepted: the connector then sends requests to the `PLUGIN_<NAME>` URL
with that entry's credential and options, so do not rely on it.) Define each tool in one place to
keep this unambiguous.

### Applying a change without `exec`

Neither path needs `exec` to take effect:

- **`plugins:` values.** `helm upgrade` changes the Deployment's pod template, so Kubernetes rolls
  out a replacement pod by itself. The connector reads `PLUGIN_*` only at startup; a `SIGHUP` does
  not re-read them. (With Docker, where the variables come from an env file, recreate the container
  after editing the file: `docker restart` keeps the old environment.)
- **Mounted Secret.** Updating the Secret refreshes the mounted file in the pod after the kubelet's
  delay, but the connector reads that file only at startup and on `SIGHUP`, and the chart adds
  nothing that restarts the pod when the Secret changes. Restart the workload:

  ```bash
  kubectl rollout restart deployment/mkonnect --namespace "$NAMESPACE"
  ```

  This needs permission to patch Deployments, not `pods/exec`.

To check the result without `exec`, read the connector's logs
(`kubectl logs deployment/mkonnect --namespace "$NAMESPACE"`, which needs `pods/log`) and use
**Test connection**, as described in [Verify a tool from the portal](#verify-a-tool-from-the-portal).

## How your credentials are stored

Be clear-eyed about where these secrets live:

- They are written to a JSON file on the connector's own volume — `/data/credentials.json` by
  default, or wherever `CREDS_FILE` points. The file is created with mode `0600` (owner-only) and
  owned by the non-root container user (uid `65532`), and is written atomically.
- **Tokens are stored in cleartext.** mkonnect does not encrypt them at rest and does not integrate
  with an external secret manager or OS keychain on its own. Anyone who can read that volume — a
  `docker exec` into the container, `root` on the host reading the Docker named volume, or a backup
  or snapshot of it — can read the tokens. Treat the connector host and its `/data` volume as
  sensitive: restrict who can reach them, and encrypt (or exclude) that volume in your backups.
- As covered in the trust boundary above, the tokens never leave your network — they are injected
  into the outbound request on-prem and are never sent to the Memento platform.

If your platform provides a secret manager (HashiCorp Vault, a cloud secret store, Kubernetes
Secrets), prefer sourcing the credential file from it rather than typing tokens into a long-lived
container. On Kubernetes, mount a Secret as the credential file — see below.

### Mounting a Secret as the credential file (Kubernetes)

Instead of running `connector config set` against a live pod, keep the credentials in a `Secret`
you manage — which can itself be backed by Vault, the External Secrets Operator, or a cloud secret
store — and mount it into the connector. The durable copy then lives in that Secret, never in your
Helm values or shell history. The creation step uses a short-lived local file, so create it with
restrictive permissions outside your checkout and remove it after the Secret has been created.

1. Build the credential JSON (same shape the CLI writes) and load it into a Secret:

   ```bash
   umask 077
   mkonnect_secret_dir="$(mktemp -d)"
   chmod 700 "$mkonnect_secret_dir"
   credentials_file="$mkonnect_secret_dir/credentials.json"
   : > "$credentials_file"
   chmod 600 "$credentials_file"
   cat > "$credentials_file" <<'EOF'
   {
     "jenkins": {
       "base_url": "http://jenkins.internal:8080",
       "auth": "basic",
       "username": "REPLACE_WITH_USERNAME",
       "token": "REPLACE_WITH_API_TOKEN"
     }
   }
   EOF
   # Replace the REPLACE_WITH_* placeholders in a local editor; do not paste a real token into this shell block.
   ${EDITOR:-vi} "$credentials_file"
   kubectl create secret generic mkonnect-tool-creds --namespace "$NAMESPACE" \
     --from-file=credentials.json="$credentials_file" \
     && rm "$credentials_file"
   ```

   If creating the Secret fails, the owner-only file remains in the temporary directory so you can
   correct the problem and retry. Do not add it to source control. Create the Secret in the
   namespace of the release, because a pod can mount only Secrets from its own namespace (see
   [Use one namespace for every command](installation.md#use-one-namespace-for-every-command)).

   Each entry's fields are the same as the CLI options: `base_url` (required), `auth` (`bearer`,
   `basic`, or omit for none), `username` (basic auth only), `token` (at least 16 characters), an
   optional `strip_headers` (a list of extra inbound headers to drop — see below), an optional
   `strip_query_params` (a list of extra inbound query-parameter names to drop — see below), an
   optional `ca_bundle` (an absolute path to a PEM bundle of extra root CA(s) to trust for this tool's
   upstream TLS — see [Trusting an internal CA](#trusting-an-internal-ca-custom-ca-bundle)), and an
   optional `upstream_timeout` (a per-tool request timeout — see
   [Tuning the upstream timeout](#tuning-the-upstream-timeout)).

2. Point the chart at the Secret in your values:

   ```yaml
   credentials:
     secretName: mkonnect-tool-creds
     key: credentials.json
   ```

The chart then mounts the Secret **read-only** at `/etc/mkonnect/credentials.json` (mode `0440`, so
the non-root connector can read it via its `fsGroup`) and sets `CREDS_FILE` accordingly. Because the
mount is read-only, `connector config set` cannot write there — manage credentials by updating the
Secret and restarting the workload
(`kubectl rollout restart deployment/mkonnect --namespace "$NAMESPACE"`). Leaving
`credentials.secretName` empty keeps the default behavior (a writable `/data/credentials.json` you
manage with the CLI).

## Trusting an internal CA (custom CA bundle)

An on-prem tool often presents a TLS certificate signed by an **internal certificate
authority** rather than a public one. By default the connector verifies upstream TLS
against the host's system trust store, so such a certificate fails verification and the
tool is unreachable. Point the connector at a PEM bundle of your internal CA(s) and it
trusts them **in addition to** the system roots.

The connector **never disables TLS verification.** There is deliberately no
"insecure"/"skip-verify" switch — the only supported way to reach an internal-CA tool is
to supply its CA. A bundle that cannot be read (missing, not a regular file, or holding
no certificate) is a hard error: requests for the affected tool fail closed with an
`invalid_ca_bundle` marker rather than silently falling back to the system store.

Configure it one of two ways:

- **Globally** — set `CA_BUNDLE` to the absolute path of the PEM file (the Helm chart
  does this for you when you mount a bundle, below). It applies to every tool that does
  not set its own.
- **Per tool** — set `--ca-bundle` (to an absolute path) on `connector config set`, or the
  `ca_bundle` field in the credentials file. A per-tool bundle overrides the global one
  for that tool.

```bash
mkonnect_exec connector config set gitscm \
  --base-url https://gitscm.internal \
  --auth bearer --token-stdin \
  --ca-bundle /etc/mkonnect-ca/ca-bundle.pem
```

The path must be **absolute** (the long-running connector resolves it with its own
working directory). As with other credential changes, a running connector caches this in
memory: reload it (`docker kill -s HUP mkonnect` or
`kubectl rollout restart deployment/mkonnect --namespace "$NAMESPACE"`) to apply a new or rotated
bundle — see
[Applying changes](#applying-changes).

**With credentialed tools and the proxy.** A tool that has a local credential is reached
over a *direct* connection that bypasses the ambient `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`
(so the token goes straight to the tool, never to a proxy); the CA bundle is applied to
that direct connection. An unauthenticated tool still honours the ambient proxy settings,
and the CA bundle is applied there too — including to the TLS connection to an
`https://` proxy, so if a TLS-intercepting proxy sits in front of the tool, that proxy's
CA must also be in the bundle (or the system store). The connection test shown in the
portal verifies the same way, so a tool reachable at runtime reports `connected` rather
than a TLS failure.

### Mounting the CA bundle (Kubernetes)

CA certificates are public, non-secret data, but mounting them from a `Secret` keeps the
wiring consistent with the credential mount. Create a Secret holding the PEM bundle and
point the chart at it:

```bash
kubectl create secret generic mkonnect-ca --namespace "$NAMESPACE" \
  --from-file=ca-bundle.pem=/path/to/internal-ca-bundle.pem
```

```yaml
caBundle:
  secretName: mkonnect-ca
  key: ca-bundle.pem
  mountPath: /etc/mkonnect-ca
```

The chart mounts the Secret **read-only** at `<mountPath>/<key>` (mode `0444`) — a sibling
of the credentials mount, not nested inside it — and sets `CA_BUNDLE` to that path so the
bundle applies to every tool by default. To scope a different bundle to a single tool,
set that tool's `ca_bundle` to another absolute path instead. Leaving
`caBundle.secretName` empty keeps the default behavior (system trust store only).

## Running the connector CLI

The `connector` command is a **subcommand of the `/mkonnect` binary**, not a separate
executable. Because `docker exec` and `kubectl exec` bypass the image's entrypoint, you must
invoke the binary by its path — `/mkonnect connector ...`. Running `connector` on its own fails
with `executable file not found in $PATH`.

Run it inside the already-running container. Define a small shell function once, so that the
examples below read `mkonnect_exec connector ...`. Run only the definition that matches how you
installed the connector (the Bash or Zsh session keeps it until you close it, so define it again in
a new one).

**Docker** (change `mkonnect` if your container has another name):

```bash
mkonnect_exec() { docker exec -i mkonnect /mkonnect "$@"; }
```

**Kubernetes** (`kubectl exec` picks a pod of the `mkonnect` Deployment, for a release named
`mkonnect`; `NAMESPACE` is set as in [Set your values once](#set-your-values-once)):

```bash
mkonnect_exec() { kubectl exec -i --namespace "${NAMESPACE:?set NAMESPACE}" deploy/mkonnect -- /mkonnect "$@"; }
```

If your cluster does not allow `pods/exec`, see
[Configuring tools without `kubectl exec`](#configuring-tools-without-kubectl-exec).

The `-i` (interactive) flag in both definitions matters: tokens are read from **stdin**, never
from a command-line flag.

## Configure an unauthenticated tool

For an open endpoint like a Prometheus with no auth, either set a `PLUGIN_<NAME>` variable at
install time, or register it with the CLI (base URL only):

```bash
mkonnect_exec connector config set prometheus --base-url http://prometheus.internal:9090
```

### Reaching a tool from Docker

With Docker's normal bridge networking, the base URL is resolved from mkonnect's network namespace.
In particular, `localhost` means the mkonnect container itself, not the Docker host where you ran
the command.

- For a tool running on the Docker host, use `http://host.docker.internal:8080` (with your tool's port) on Docker
  Desktop (macOS or Windows). On Linux, add
  `--add-host host.docker.internal:host-gateway` to the `docker run` command that starts mkonnect,
  then use the same URL.
- On Linux, `--network host` is an alternative when your deployment policy permits it. In that mode
  `http://localhost:8080` (with your tool's port) reaches the Docker host. Use the same network option whenever you
  recreate mkonnect, including when removing the registration token.
- For a tool in another container, attach both containers to the same user-defined Docker network
  and use that container's network alias in the base URL. For example:

  ```bash
  docker network create mkonnect-tools
  docker network connect mkonnect-tools mkonnect
  docker network connect --alias jenkins mkonnect-tools "${TOOL_CONTAINER:?set TOOL_CONTAINER to the name of the tool container}"
  ```

  Set `TOOL_CONTAINER` to the name of the tool's container first (it is the name `docker ps` shows).

  The tool is then reachable at `http://jenkins:8080` if it listens on port 8080. Do not rely on
  Docker's default bridge network for container-name DNS. Reattach mkonnect to this network after
  every container recreation or image upgrade: `docker network connect mkonnect-tools mkonnect`.

## Configure a bearer-token tool

For a tool whose API authenticates with a bearer token, pipe the token through stdin so it never
appears in the process list or your shell history. Grafana (service-account tokens) is used here
only as an example of such a tool:

```bash
printf 'Paste the Grafana token: ' >&2
IFS= read -r -s GRAFANA_TOKEN
printf '\n' >&2
printf %s "$GRAFANA_TOKEN" | \
  mkonnect_exec connector config set grafana \
    --base-url http://grafana.internal:3000 \
    --auth bearer --token-stdin
unset GRAFANA_TOKEN
```

The connector will add `Authorization: Bearer <token>` to each request it forwards to `grafana`.
Use this mode only for tools that actually accept `Authorization: Bearer` — Jenkins does not; see
the basic-auth example below.

## Configure a basic-auth tool

For a tool using HTTP Basic authentication, provide a username and pipe the password/token through
stdin. **Jenkins is used here as the example, because it is a common source of confusion:** its
REST API authenticates with HTTP Basic — your Jenkins username plus an API token used as the
password — and does **not** accept `Authorization: Bearer`. Configuring Jenkins as a bearer tool
makes every request fail with `401`/`403` even though the connector shows as connected.

```bash
# Your Jenkins username.
JENKINS_USERNAME=''
printf 'Paste the Jenkins API token: ' >&2
IFS= read -r -s JENKINS_API_TOKEN
printf '\n' >&2
printf %s "$JENKINS_API_TOKEN" | \
  mkonnect_exec connector config set jenkins \
    --base-url http://jenkins.internal:8080 \
    --auth basic --username "${JENKINS_USERNAME:?set JENKINS_USERNAME}" --token-stdin
unset JENKINS_API_TOKEN
```

The connector adds the `Authorization: Basic …` header, encoding `username:token` locally — here
the Jenkins API token is the `token` and is sent as the password.

## Token rules

- **Stdin only.** Tokens must be supplied through `--token-stdin`. A `--token <value>` flag is
  rejected with an error, so a secret can't leak into shell history or `ps` output.
- **Minimum length.** Tokens must be at least 16 characters.
- **Trailing newline is trimmed**, so `printf %s` and `echo` both work; prefer `printf %s` to avoid
  a stray newline in the first place.
- A username (basic auth) must not contain a `:`.

## Manage configured tools

```bash
# Base URLs; credentials are shown masked:
mkonnect_exec connector config list
# This connector's configured plugins:
mkonnect_exec connector status
mkonnect_exec connector config remove jenkins
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
  kubectl rollout restart deployment/mkonnect --namespace "$NAMESPACE"
  ```

Either way the connector then pushes a fresh status report to the portal. Restarting the Docker
container (`docker restart mkonnect`) also works and is equivalent to a reload.

## Verify a tool from the portal

After configuring a tool, open **Integrations → On-Prem Connector** (or the tool's integration
card) in the Memento portal:

- The connector reports each plugin's state — **configured & connected**, **configured (error)**,
  or **not configured** — on connect, whenever the local config changes, and on each heartbeat.
  The connector learns a tool's state only when it is tested, and keeps the result in memory, so
  every tool is reported as **not configured** after each start or restart until you use
  **Test connection** on it.
- Use **Test connection** to trigger an on-demand connectivity probe. The connector sends a `HEAD` request
  to the tool's base URL (redirects are not followed) and reports the result: `connected`,
  `auth_failure`, `unreachable`, or `timeout`. The request carries the tool's local credential, if
  one is configured, but the credential is never included in the result sent to Memento. Only a
  401 or 403 reads as `auth_failure`; any other 2xx or 3xx answer, a 404 or a 405 reads as
  `connected`. The test therefore shows that the base URL answers, not that the credential is
  right: a tool that answers its base URL without asking for a credential, or redirects it to a
  sign-in page, reads `connected` even with a wrong token.

## Tuning the upstream timeout

By default the connector gives each forwarded request **10 seconds** to complete — connecting,
sending, and reading the response. Some tools legitimately take longer (a large commit, a report
or search endpoint on a slow self-managed instance). Raise the timeout **per tool** with
`--upstream-timeout`, a Go duration with a unit between `1s` and `10m`:

```bash
mkonnect_exec connector config set gitscm \
  --base-url https://scm.internal \
  --auth bearer --token-stdin \
  --upstream-timeout 2m
```

For an unauthenticated (`PLUGIN_<NAME>`) tool, register a credential entry with the base URL only
and the timeout — it needs no `--auth`:

```bash
mkonnect_exec connector config set reports --base-url http://reports.internal:8080 --upstream-timeout 45s
```

Omitting the flag keeps the 10 s default; the equivalent in a mounted Secret is the
`upstream_timeout` field. A change takes effect after a [reload](#applying-changes). Notes and
limits:

- The override applies only to **forwarded requests**, not to the **Test connection** probe, which
  keeps its own fixed timeout.
- It bounds the whole request, but the connect phase (30 s) and the TLS handshake (10 s) keep their
  own fixed ceilings, so a larger value does not extend those phases.
- The platform stops waiting at its own caller deadline; raising this timeout does not make the
  platform wait longer. The connector does not shorten an in-flight request to that deadline, so a
  long value keeps a worker slot occupied for its full duration, and a request still running when
  the connector drains on shutdown (a 10 s window) may not return a reply.
- A value above `10m` (or below `1s`) is rejected — the bounds are a guardrail against a typo, not
  a tuning knob, so that a single request cannot hold a connector worker slot for an unreasonable
  time. In a mounted Secret, an out-of-range or unit-less `upstream_timeout` is rejected when the
  file loads, exactly like a malformed `token` or `ca_bundle`.

See the [project README's Limits section](../README.md#limits) for the connector's other
(fixed) limits — concurrency and the request/response body caps.

## Troubleshooting

| Result | Meaning and fix |
|---|---|
| `not configured` | No `PLUGIN_<NAME>` variable and no credential entry for this plugin name. Add one: `connector config set`, an entry in the mounted Secret, or `plugins.<name>` in the Helm values (see [Configuring tools without `kubectl exec`](#configuring-tools-without-kubectl-exec)), and confirm the name matches what the platform expects. If a tool you did configure shows `not configured` right after the connector started or restarted, run **Test connection** first: see [Verify a tool from the portal](#verify-a-tool-from-the-portal). |
| `auth_failure` (401/403) | The tool rejected the credential. Re-check the token/username and re-run `connector config set` (or update the mounted Secret and restart the workload). For bearer tools, confirm the token has the needed scopes. |
| `unreachable` | The connector couldn't open a connection to the base URL. Confirm the URL and port are correct and reachable from the connector's network location, and that DNS resolves. For Docker, remember that `localhost` is the connector container; see [Reaching a tool from Docker](#reaching-a-tool-from-docker). Diagnostics are intentionally coarse (no internal addresses are sent to the platform). |
| `timeout` | The tool accepted the connection but didn't respond in time. Check the tool's health and any intermediate proxy. If the tool is simply slow (large commits, a slow self-managed instance), raise its request timeout — see [Tuning the upstream timeout](#tuning-the-upstream-timeout). (The **Test connection** probe itself uses a fixed timeout and is not affected by that setting.) |
| Credential change didn't take effect | The running connector caches credentials in memory. Reload it after a change: `docker kill -s HUP mkonnect` (Docker) or `kubectl rollout restart deployment/mkonnect --namespace "$NAMESPACE"` (Kubernetes). See [Applying changes](#applying-changes). |
| A credentialed tool is unreachable but a proxy is set | Requests that carry a **local credential** deliberately bypass ambient `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`, so the token goes straight to the tool rather than to a proxy. Ensure the connector has *direct* network access to the tool's host. (Unauthenticated `PLUGIN_<NAME>` requests still honor the ambient proxy settings.) |

For connector-level connection problems (the bridge itself showing degraded), see the
[Installation guide's troubleshooting](installation.md#troubleshooting).
