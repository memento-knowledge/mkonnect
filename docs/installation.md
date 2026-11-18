# Installing the connector

This guide walks a customer administrator through deploying the mkonnect connector inside their
network and confirming it has connected to the Memento platform. Once it is running, follow the
[Setup guide](setup.md) to make your internal tools reachable.

For a conceptual overview and the full environment-variable reference, see the [README](../README.md).

## Before you start

### System requirements

The connector is deliberately lightweight. A single instance needs:

- **A runtime:** Docker Engine, or a Kubernetes cluster with Helm 3.8+ (OCI support). The image is
  distroless and runs as a non-root user (uid 65532).
- **CPU architecture `linux/amd64`.** The published image is amd64-only. On an arm64 host (for
  example Apple Silicon) it runs under emulation — Docker Desktop's Rosetta/qemu — which works for
  evaluation but is not recommended for production; run it on an amd64 host there.
- **Modest CPU and memory.** The Helm chart defaults to a `100m` CPU / `64Mi` memory request and a
  `500m` CPU / `128Mi` memory limit — enough for its handful of concurrent in-flight requests.
  Tune these in [`charts/mkonnect/values.yaml`](../charts/mkonnect/values.yaml).
- **A small persistent volume** for the connector's private key and local credential store (a
  Docker volume, or a `PersistentVolumeClaim`; the chart default is `1Gi`, though it uses far
  less). The key is issued once at registration and reused on every reconnect; losing it means
  re-registering.
- **Outbound HTTPS (443) only.** The connector dials *out* to the Memento Bridge Gateway (`wss://`)
  and to Memento's container registry. It never accepts inbound connections, so no inbound firewall
  rules or port-forwarding are required. It also needs outbound access to whichever internal tools
  you want to bridge (e.g. Jenkins, Prometheus) on their own ports.

**Running several tools through one connector.** A single connector can bridge many tools at once —
each is just another plugin name in its registry and credential store — and its footprint is driven
by concurrent request volume and payload size, not by how many tools are configured. **Prefer one
connector for all of your tools** rather than one per tool; run separate connectors only when your
tools sit in different network zones (each connector must be able to reach its tools' base URLs) or
you want their credentials kept isolated. Two limits apply to a connector regardless of how many
tools it serves:

- **Concurrency.** A connector serves up to four in-flight requests at a time (the `128Mi` memory
  limit is sized for this); heavier parallel load queues behind them rather than using more memory.
- **Response size.** Each request and response body is capped at 32 MiB. A tool that can return
  large payloads — a broad log or search query, for instance — may exceed this and receive a `502`;
  scope or paginate such queries to stay under the cap.

### What you'll collect from the Memento portal

Open the Memento portal, go to **Integrations → On-Prem Connector**, and note:

| Value | Used as | Notes |
|---|---|---|
| Gateway URL | `GATEWAY_URL` | Copy it **exactly** from the portal; do not construct it from an example URL. |
| Connector ID | `CONNECTOR_ID` | a UUID identifying this connector |
| Registration token | `REGISTRATION_TOKEN` (or `REGISTRATION_TOKEN_FILE`, a path to a file holding it) | **one-time**, expires in 24h — used only on first start |
| Release ID / chart version | `RELEASE_ID` (Docker) / `CHART_VERSION` (Helm), both optional | **Not needed:** without one, you get the newest release. Set one only to pin a release; see [Choosing a release](#choosing-a-release). |

The registration token is a one-time secret. It is consumed on first startup; after that the
connector authenticates with the private key it was issued and the token is no longer needed.

`REGISTRATION_TOKEN_FILE` is an alternative to `REGISTRATION_TOKEN`: an absolute path to an
owner-only file (group read allowed, no "other" access) that holds the token. When it is set, the
file is used and `REGISTRATION_TOKEN` is ignored. If the file is missing or unreadable, startup fails
rather than falling back, so once the connector has registered, keep the file in place or unset
`REGISTRATION_TOKEN_FILE` before deleting it. On Kubernetes, mount the `Secret` with `defaultMode`
`0440` or `0400` and set the pod `fsGroup`; the default `0644` is rejected. The Helm chart in this
repository still injects `REGISTRATION_TOKEN` through the environment; file support for the chart
will follow separately.

> **Security note:** `GATEWAY_URL` must be `wss://` (TLS). The connector refuses a plaintext
> `ws://` gateway unless you explicitly set `ALLOW_INSECURE_GATEWAY=true` *and* the host is
> loopback — an escape hatch intended only for local development.

### Set your values once

The commands in this guide read your values from shell variables, so you can copy them as
written: a value you have to type into a command itself, such as `<gateway-url>`, would be read by
the shell as input redirection and the command would fail. Set the variables first, in the Bash or
Zsh session you will run the commands from:

```bash
# The Gateway URL from the portal, copied exactly. It starts with wss://
GATEWAY_URL=''
# The Connector ID from the portal.
CONNECTOR_ID=''
# Kubernetes only: the namespace for the connector. Any name you choose.
NAMESPACE=mkonnect
```

A command that needs `GATEWAY_URL` or `CONNECTOR_ID` stops with a message while the variable is
empty, instead of running with a missing value; read that message and fix it before you continue
with the next command. The variables last only for the current shell session: if you open a new
one for a later step (verifying, removing the token, upgrading), set `NAMESPACE` again first,
because a `helm` or `kubectl` command with an empty namespace silently uses your current context's
namespace. If you use Zsh, run `setopt interactivecomments` once in the session first, so that the
`#` comments in the commands are ignored as they are in Bash.

### Choosing a release

You do not need to choose one. Without a pin, the commands below install the newest published
release:

- **Docker** uses the image tag `latest`, which is the newest published release.
- **Helm** installs the newest chart, whose default image tag is its own `appVersion` (the release
  ID), so the chart version selects the matching image.

Pin a release only if you want a specific one. Releases are listed on the
[GitHub Releases page](https://github.com/memento-knowledge/mkonnect/releases), and each has a
`release-metadata.json` file that gives its release ID (`YYYYMMDD.n`) and its Helm chart version.
To pin, uncomment and fill in the variable you need before running a command. The Docker commands
use `RELEASE_ID` as the image tag; for Helm, add `--version "$CHART_VERSION"` to the command:

```bash
# Docker: the release ID.
# RELEASE_ID=YYYYMMDD.n
# Helm: the chart version of that release.
# CHART_VERSION=X.Y.Z
```

**For production, pin by digest.** `latest` moves with every release, so the same command run on
two days can start different versions. Each release's `release-metadata.json` records the digest
of its images. Fetch the file of the newest release, or of the release you set in `RELEASE_ID`, and
read the connector image's digest:

```bash
curl -fsSL -o release-metadata.json \
  https://github.com/memento-knowledge/mkonnect/releases/latest/download/release-metadata.json
# or, for the release in RELEASE_ID instead of the newest:
# curl -fsSL -o release-metadata.json "https://github.com/memento-knowledge/mkonnect/releases/download/${RELEASE_ID:?set RELEASE_ID}/release-metadata.json"
IMAGE_DIGEST=$(jq -r .connector_image_digest release-metadata.json)
printf '%s\n' "$IMAGE_DIGEST"
```

Then use `public.ecr.aws/h2a8k0r3/memento-connector@$IMAGE_DIGEST` as the image in the Docker
commands instead of the tag form. The Helm chart selects the connector image by release ID (the
chart's `appVersion`, or `image.tag`) and has no digest setting for it, so with Helm the pin is the
chart version, `CHART_VERSION`.

To see which release is running: with Docker, the `docker logs mkonnect` start line shows
`version=…`; with Helm, `helm list --namespace "$NAMESPACE"` shows the chart version and the
release ID (`APP VERSION`). Helm commands that only change a value (removing the token, adding a
tool) must not move the release to a newer chart by accident, so they ask for the chart version you
run. Set it from the installed release (needs `jq`; otherwise read it from the **CHART** column of
`helm list`, where `mkonnect-0.3.3` means `0.3.3`):

```bash
CHART_VERSION=$(helm list --namespace "$NAMESPACE" --filter '^mkonnect$' --output json | jq -r '.[0].chart | sub("^mkonnect-"; "")')
printf '%s\n' "$CHART_VERSION"
```

## Option A — Docker

Create a named volume for the key, then start the connector. Without `RELEASE_ID` it uses the
newest release (see [Choosing a release](#choosing-a-release)). Keep the one-time registration
token in a temporary, owner-only environment file rather than putting it in your shell history. Run
this snippet from Bash or Zsh, after [setting your values](#set-your-values-once):

```bash
umask 077
mkonnect_config_dir="${XDG_CONFIG_HOME:-$HOME/.config}/mkonnect"
mkdir -p "$mkonnect_config_dir"
chmod 700 "$mkonnect_config_dir"

# The guard comes first: if a value is empty, the env file is not written at all.
: "${GATEWAY_URL:?set GATEWAY_URL from the portal}" "${CONNECTOR_ID:?set CONNECTOR_ID from the portal}" &&
cat > "$mkonnect_config_dir/mkonnect.env" <<EOF
GATEWAY_URL=$GATEWAY_URL
CONNECTOR_ID=$CONNECTOR_ID
KEY_FILE=/data/key
# Optional unauthenticated internal tools belong here so upgrades and restarts keep them:
# PLUGIN_PROMETHEUS=http://prometheus.internal:9090
EOF

: > "$mkonnect_config_dir/registration.env"
chmod 600 "$mkonnect_config_dir/registration.env"
printf 'Paste registration token: ' >&2
IFS= read -r -s REGISTRATION_TOKEN
printf '\n' >&2
printf 'REGISTRATION_TOKEN=%s\n' "$REGISTRATION_TOKEN" > "$mkonnect_config_dir/registration.env"
unset REGISTRATION_TOKEN

docker volume create mkonnect-data

docker run -d --name mkonnect --restart unless-stopped --pull always \
  -v mkonnect-data:/data \
  --env-file "$mkonnect_config_dir/mkonnect.env" \
  --env-file "$mkonnect_config_dir/registration.env" \
  public.ecr.aws/h2a8k0r3/memento-connector:${RELEASE_ID:-latest}
```

If mkonnect must reach a service running on the Docker host, see [Reaching a tool from
Docker](setup.md#reaching-a-tool-from-docker) before starting it; Linux Docker requires an
additional `--add-host` flag on both `docker run` commands.

The image is a distroless, non-root, statically linked binary. The `-v mkonnect-data:/data`
volume is where the private key (`/data/key`) and the local credential store
(`/data/credentials.json`) live, so both survive container restarts and upgrades.

Put unauthenticated `PLUGIN_<NAME>` entries in `mkonnect.env` as shown above; authenticated tools
are configured after startup — see the [Setup guide](setup.md). This retained owner-only file keeps
the gateway settings and every persistent plugin setting together for restarts and upgrades.

## Option B — Kubernetes (Helm)

The chart is published as an OCI artifact, and Helm installs the newest one when you give no
`--version` (see [Choosing a release](#choosing-a-release) to pin one instead). The registration
token is a secret, so read it without echoing and pass it by file rather than `--set` (which would
land in your shell history and the process list). Run this snippet from Bash or Zsh, after
[setting your values](#set-your-values-once):

```bash
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

helm upgrade --install mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --namespace "${NAMESPACE:?set NAMESPACE}" --create-namespace \
  --set-file config.registrationToken="$mkonnect_config_dir/registration.token" \
  --set image.repository=public.ecr.aws/h2a8k0r3/memento-connector \
  --set config.gatewayUrl="${GATEWAY_URL:?set GATEWAY_URL from the portal}" \
  --set config.connectorId="${CONNECTOR_ID:?set CONNECTOR_ID from the portal}"
```

To install a specific release instead, add `--version "$CHART_VERSION"` to this command, for
example after the `--namespace` line. The image tag then follows that chart's `appVersion`; do not
also set `image.tag`.

The chart writes `registrationToken`, `gatewayUrl`, and `connectorId` into a Kubernetes `Secret`,
and provisions a `PersistentVolumeClaim` so the private key survives pod restarts. See
[`charts/mkonnect/values.yaml`](../charts/mkonnect/values.yaml) for all options (resources,
storage class, plugins, service account).

Keep the owner-only token file until [the portal confirms the connector is connected](#verify-the-connection).
If installation fails, it remains outside your checkout so you can retry without re-entering it;
the `upgrade --install` command is safe to rerun after correcting the failure, as long as you rerun
it with the same `--namespace`.

### Use one namespace for every command

Every `helm` and `kubectl` command for Kubernetes in this guide and the [Setup guide](setup.md)
carries `--namespace "$NAMESPACE"` (or `-n "$NAMESPACE"`). `NAMESPACE` is the variable you set in
[Set your values once](#set-your-values-once): use the same namespace every time, and set the
variable again in any new shell. `--create-namespace` creates the namespace if it does not exist
and needs permission to create namespaces; leave it out if the namespace already exists.

Without the flag, a command acts on whatever namespace your current kubeconfig context points at.
Helm tracks each release inside a namespace, so `helm upgrade --install` run from a different
namespace can create a second release instead of upgrading the existing one, and `kubectl` run from
a different namespace does not find the connector's Deployment, volume claim or Secrets. A Secret
you create for the connector (such as the [credential Secret](setup.md#mounting-a-secret-as-the-credential-file-kubernetes))
must be in the release's namespace, because a pod can mount only Secrets from its own namespace.

The chart sets no namespace on its objects, so everything it creates lands in the release's
namespace, and it creates no `Role` or `ClusterRole`. Installing needs permission to create the
chart's objects (a `ServiceAccount`, `Secret`, `PersistentVolumeClaim` and `Deployment`, plus
git-sync's objects if you enable it) in that namespace, and Helm by default stores its release
records there as Secrets. The commands in these guides assume a release named `mkonnect`, which gives a
Deployment named `mkonnect` and a volume claim named `mkonnect-data`. A release name that does not
contain `mkonnect` gives `RELEASE-mkonnect` and `RELEASE-mkonnect-data` instead (where `RELEASE`
stands for your release name), so adjust those names in the commands.

## Verify the connection

The connector should register and connect within a few seconds.

- **Portal:** the On-Prem Connector tab shows **bridge status: connected**, along with the
  connector version and a recent heartbeat.
- **Docker logs:**

  ```bash
  docker logs mkonnect
  ```

  A healthy start logs `mkonnect starting — connector=… gateway=… protocol=… version=…` and then
  no repeating `gateway connection failed` warnings.
- **Inside the container:**

  With Docker:

  ```bash
  docker exec -i mkonnect /mkonnect connector status
  ```

  With Kubernetes:

  ```bash
  kubectl exec -i --namespace "$NAMESPACE" deploy/mkonnect -- /mkonnect connector status
  ```

  If your cluster does not allow `pods/exec`, skip this check: use the portal status above, or read
  the pod's logs with `kubectl logs deployment/mkonnect --namespace "$NAMESPACE"` (which needs
  `pods/log`). To configure tools without `exec`, see
  [Configuring tools without `kubectl exec`](setup.md#configuring-tools-without-kubectl-exec).

If the status stays **degraded**, see [Troubleshooting](#troubleshooting).

Next: [set up your internal tools](setup.md).

## Remove the registration token after verification

Only remove the registration token after the portal has shown **connected**. Until then, retain the
owner-only file so a failed start can be retried without requesting another token.

**Docker:** Docker stores environment variables in the container configuration, so recreate the
container without the one-time token. All persistent settings, including `PLUGIN_<NAME>` entries,
remain in `mkonnect.env` and are loaded again:

```bash
mkonnect_config_dir="${XDG_CONFIG_HOME:-$HOME/.config}/mkonnect"
mkonnect_image=$(docker inspect --format '{{.Config.Image}}' mkonnect)
rm "$mkonnect_config_dir/registration.env"
docker rm -f mkonnect
docker run -d --name mkonnect --restart unless-stopped \
  -v mkonnect-data:/data \
  --env-file "$mkonnect_config_dir/mkonnect.env" \
  "$mkonnect_image"
```

The command reads the image reference from the running container before removing it, so the
replacement runs the same image reference, with no need to remember a `RELEASE_ID` from an earlier
shell. (A container started from `latest` is recreated from the copy of `latest` on the host,
which does not change until you pull.) The persisted key in `mkonnect-data` lets the replacement container reconnect without registering
again. Include the same Linux Docker networking option, if any, that you used for the first run.
If you attached mkonnect to a user-defined network for a sibling tool, reattach it after each
recreation, with `TOOL_NETWORK` set to that network's name:

```bash
docker network connect "${TOOL_NETWORK:?set TOOL_NETWORK to the network name}" mkonnect
```

**Kubernetes:** remove the local token file, then clear the active Helm value. This command only
changes a value, so it asks for the chart version you run: set `CHART_VERSION` as shown in
[Choosing a release](#choosing-a-release) first. Without `--version`, Helm would move the release
to the newest chart as well. If the release has [git-sync](git-sync.md) enabled, use
`--reset-then-reuse-values` instead of `--reuse-values` (see the
[git-sync Helm notes](git-sync.md#option-b-kubernetes-helm)):

```bash
mkonnect_config_dir="${XDG_CONFIG_HOME:-$HOME/.config}/mkonnect"
rm "$mkonnect_config_dir/registration.token"
helm upgrade mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --namespace "$NAMESPACE" --version "${CHART_VERSION:?set CHART_VERSION to the chart version you run}" \
  --reuse-values \
  --set config.registrationToken=""
```

This clears the token from the active release value but does **not** redact earlier Helm release
revisions, which can remain accessible to users with permission to read Helm release metadata.
The token is one-time and consumed, but restrict access to the cluster Secret and every Helm
revision. If policy requires the token to be absent from all retained release metadata, involve the
cluster administrator in the release-history retention procedure before deployment.

## Upgrading

Releases use calendar identifiers (`YYYYMMDD.n`). To move to the newest release, run the commands
below without a pin. To move to, or stay on, a specific release, set `RELEASE_ID` (Docker) or
`CHART_VERSION` (Helm); for production, pin by digest as described in
[Choosing a release](#choosing-a-release), because `latest` moves with every release. Each Helm
chart version's default image tag is its `appVersion`, so the chart version selects the matching
image.

**Docker:** pull the image and recreate the container with the same volume. The `docker pull` is
what moves `latest` to the newest release: `docker run` alone reuses the image already on the host.

```bash
mkonnect_config_dir="${XDG_CONFIG_HOME:-$HOME/.config}/mkonnect"
docker pull public.ecr.aws/h2a8k0r3/memento-connector:${RELEASE_ID:-latest}
docker rm -f mkonnect
docker run -d --name mkonnect --restart unless-stopped \
  -v mkonnect-data:/data \
  --env-file "$mkonnect_config_dir/mkonnect.env" \
  public.ecr.aws/h2a8k0r3/memento-connector:${RELEASE_ID:-latest}
```

Because the key and credential store live on the `mkonnect-data` volume, the connector reconnects
with its existing key — no re-registration and no credential re-entry. Include the same Linux
Docker networking option used at installation, and reattach any user-defined sibling-tool network
with `docker network connect "$TOOL_NETWORK" mkonnect` (the network's name in `TOOL_NETWORK`).

**Helm:** to move to the newest release, give no `--version`:

```bash
helm upgrade mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --namespace "$NAMESPACE" --reuse-values
```

To move to, or stay on, a specific release, add its chart version (see
[Choosing a release](#choosing-a-release)):

```bash
helm upgrade mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --namespace "$NAMESPACE" --version "${CHART_VERSION:?set CHART_VERSION}" --reuse-values
```

If the release has [git-sync](git-sync.md) enabled, use `--reset-then-reuse-values` instead of
`--reuse-values`, as the [git-sync upgrade steps](git-sync.md#upgrading) explain.

To roll back a bad upgrade, find the revision to return to, then roll back to it:

```bash
helm history mkonnect --namespace "$NAMESPACE"
helm rollback mkonnect "${REVISION:?set REVISION to a revision number from the history}" --namespace "$NAMESPACE"
```

Each GitHub Release also carries the chart `.tgz` and a `.sha256` checksum; if you install from the
downloaded archive instead of OCI, verify it first with `sha256sum -c` on the downloaded `.sha256`
file, run from the directory that holds both files.

## Uninstalling

**Docker:**

```bash
docker rm -f mkonnect
docker volume rm mkonnect-data     # also removes the private key and local credentials
```

**Helm:**

```bash
helm uninstall mkonnect --namespace "$NAMESPACE"
# the PersistentVolumeClaim may be retained by your storage class; delete it explicitly
# if you want to discard the key:
kubectl delete pvc mkonnect-data --namespace "$NAMESPACE"
```

Because credentials for your internal tools are stored on the connector's local volume
(see the [Setup guide](setup.md)), removing the connector and its volume removes them from your
network entirely — there is nothing to revoke on Memento's side. (If you instead mount a
Kubernetes Secret as the credential store, delete that Secret too — it is managed outside the
connector's volume and is not removed by `helm uninstall`.)

## Troubleshooting

| Symptom | Likely cause and fix |
|---|---|
| Container exits immediately with a config error | A required variable is missing or malformed. `GATEWAY_URL` must be an absolute `wss://` URL and `CONNECTOR_ID` must be set. Check `docker logs mkonnect`. |
| `GATEWAY_URL must use wss://` | The gateway URL is plaintext `ws://`. Use `wss://`. For local development only, a loopback `ws://` URL is allowed with `ALLOW_INSECURE_GATEWAY=true`. |
| Repeating `gateway connection failed` warnings | The connector can't reach the gateway. Confirm outbound HTTPS (443) to the host in the Gateway URL from the portal is allowed by your firewall/proxy. The connector retries with backoff (1s → 60s), so transient failures self-heal. |
| Portal shows **degraded** after it was connected | The connector lost its heartbeat (stopped, or lost outbound access). Verify the container/pod is running and can still reach the gateway. |
| First start fails with a registration error | The registration token is expired (24h) or already consumed. Generate a fresh token in the portal and restart. If an earlier attempt failed with `permission denied` while saving the key (below), the token was consumed then — you'll need a fresh one. |
| `cannot register: key directory … is not writable` | The connector refuses to register when it can't persist the key, so it doesn't waste the one-time token. This means the `/data` volume isn't writable by the container's non-root user (uid 65532). An **empty** named volume with a current image is writable automatically; if you reuse a **non-empty** volume created by an older image (or a bind mount, which Docker never initializes from the image), fix its ownership once: `docker run --rm -v mkonnect-data:/data alpine chown -R 65532:65532 /data`, then restart. |
| `helm` or `docker pull` fails with `403 … authorization token has expired` (or another authorization error) for `public.ecr.aws` | A stale login to `public.ecr.aws` (for example from an earlier `helm registry login` or `docker login`) is sent with the request and rejected. The chart and images are public and need no login: run `helm registry logout public.ecr.aws` and `docker logout public.ecr.aws`, then try again. |
| `key file has unsafe permissions` | The persisted key file is group/world-accessible. The connector requires owner-only (`0600`) access; fix the volume's permissions. |

For issues configuring the tools the connector proxies (auth failures, unreachable services), see
the [Setup guide's troubleshooting](setup.md#troubleshooting).
