# Installing git-sync

git-sync is an optional add-on to mkonnect. It keeps a read-only copy of selected internal git
repositories in Memento by mirroring their branches and tags from your git server to a Memento
replica. Like the API connector, it runs inside your network and only dials **out**; nothing
needs to be exposed to the internet.

This guide covers installing, configuring, verifying, upgrading, and removing git-sync, with
Docker or Kubernetes (Helm). For a review of what it connects to, what it stores, and how it is
hardened, see [git-sync security and data handling](git-sync-security.md).

git-sync is a **separate connector** from the API connector described in the
[Installation guide](installation.md). It has its own image, its own connector ID, its own
registration token, its own private key, and its own volume. You can run both on the same host or
cluster; they share nothing.

## How it works

For each repository that you configure in the Memento portal, git-sync:

1. **Fetches** the repository's branches and tags from your git server into a bare mirror on its
   local volume. It only ever *reads* from your git server.
2. **Pushes** those branches and tags from the mirror to the Memento replica over HTTPS. A branch
   or tag deleted on your git server is deleted on the replica at the next sync.

git-sync syncs a repository:

- when the platform sends or refreshes the repository configuration (every repository in it that
  can be synced is synced, which excludes repositories whose host you have not configured);
- when the platform asks for an immediate sync; and
- **periodically**, on its own timer. By default each repository is synced again 15 minutes after
  its previous sync finished, and failures are retried with a growing delay. See
  [Sync schedule and retries](#sync-schedule-and-retries).

The first sync of a repository downloads all of its history; later syncs transfer only what
changed.

git-sync does not decide which repositories to sync: the platform sends the full list, and
git-sync syncs nothing until it has received one. When a repository disappears from that list, git-sync
removes its local mirror after a grace period (see [Mirror cleanup](#mirror-cleanup)).

Your git server's credential stays on your side: it is stored in git-sync's local configuration
and used only for requests to your git server. It is never sent to Memento.

## Before you start

### Requirements

- **A runtime:** Docker Engine, or a Kubernetes cluster with **Helm 3.14 or newer** (the Helm
  commands below use `--reset-then-reuse-values`, which was added in 3.14). The API connector's
  own guide still supports Helm 3.8+.
- **A git-sync connector** registered for your tenant, following the installation instructions you
  were given. It has its own connector ID, which is **not** the API connector's.
- **A git server reachable over HTTP or HTTPS** from the host that runs git-sync. SSH remotes are
  not supported. A credential (bearer token, or username and token for HTTP Basic) can only be used
  with an `https://` server.
- **A service account on your git server** with read access to the repositories you want to
  replicate, and nothing else. See [Use a least-privilege git account](git-sync-security.md#use-a-least-privilege-git-account).
- **Outbound HTTPS (443)** to the Memento Bridge Gateway, to the Memento replica host, and to your
  git server(s), plus the container registry when you install or upgrade. No inbound ports are
  needed or opened. See [Network destinations](git-sync-security.md#network-destinations).
- **Disk.** A persistent volume holding a mirror of every replicated repository, and separate
  scratch space for git (see [Sizing](#sizing)).
- **Memory and CPU.** The Helm chart defaults to a `250m` CPU / `512Mi` memory request and a `1`
  CPU / `2Gi` memory limit. Very large repositories may need more memory.

### What you'll collect

| Value | Used as | Notes |
|---|---|---|
| Gateway URL | `GATEWAY_URL` | Copy it **exactly** as given; it must be `wss://`. |
| Connector ID | `CONNECTOR_ID` | The ID of the **git-sync** connector, not the API connector's. |
| Replica host | `GIT_SYNC_REPLICA_HOST` | A host name or `host:port`; no `https://`, no path. The port defaults to 443. git-sync pushes only to this host and port. |
| Registration token | read from a file | **One-time**, used only on first start. |
| Release ID / chart version | image tag / `--version` | Pin an explicit version; never a moving tag. |

The registration token is a one-time secret: it is consumed on first startup, after which the
connector authenticates with the private key it was issued. git-sync reads the token **from a
file** (`REGISTRATION_TOKEN_FILE`), not from an environment variable, so it does not show up in
`docker inspect` or in the pod specification. The file must be a regular file of at most 4 KiB that
is not accessible by others and not writable by its group.

### Check that the release has a git-sync image

The git-sync image is built and published separately from the API connector image and the Helm
chart, and a release can exist without one. Before you install or upgrade, check the release:

```bash
gh release download <release-id> --repo memento-knowledge/mkonnect \
  --pattern release-metadata.json --dir .
jq '{git_sync_release_id, git_sync_image_digest}' release-metadata.json
```

(or open `release-metadata.json` from the GitHub Release page). If `git_sync_release_id` is
`null`, no git-sync image was published for that release: do not install it, and wait for a later
release. Otherwise, **pin the image by digest** (`git_sync_image_digest`) rather than by tag; the
commands below show both forms. If a pod or container fails to start with an "image not found"
error, the release most likely has no git-sync image.

### Sizing

- **Data volume** (`/data`): the sum of the mirrored repositories' sizes, multiplied by about 1.2,
  plus headroom for growth. A mirror holds the full history of every branch and tag, so it is about
  the size of a bare clone. Git writes the objects it fetches into the mirror, and repacks them
  there, so this is the volume that needs headroom. The Helm chart default is `20Gi`. The mirror of
  a repository that leaves the configuration is kept for at least seven days before it is removed
  (see [Mirror cleanup](#mirror-cleanup)), so leave room for those as well.
- **Scratch space** (`/tmp`): holds each sync's empty home directory and git's own temporary
  files, not the mirrors, so it is much smaller than the data volume. It must be backed by disk,
  not memory. The Helm chart default is a `5Gi` disk-backed `emptyDir`; raise
  `gitSync.tmp.sizeLimit` if you see `local_disk_exhausted` for `/tmp`.
- **Concurrency and memory:** by default up to four repositories sync at once; the others wait their turn
  (see [Concurrency](#concurrency)). A very large repository, or a large first sync, can exceed the
  default memory limit; the repository is then reported as `job_out_of_memory` (see
  [Troubleshooting](#troubleshooting)).

## Option A: Docker

Run git-sync as a second container, with its **own** volumes. Do not reuse the API connector's
volume. The container is started read-only with no Linux capabilities, and with a separate
disk-backed volume for scratch space at `/tmp`.

The one-time registration token is kept in a small Docker volume, `gitsync-secrets`, instead of on
the host file system or in an environment variable. git-sync refuses a token file that others can
access, so the file must be owned by the container user (uid 65532) with owner-only permissions.
The steps below create it that way without needing root on the host: a one-off container, run
from the git-sync image as its own user, writes the token from standard input, so it never appears
on a command line or in your shell history. (The image ships `/data` owned by that user, and Docker
initialises a new, empty named volume from it, which is why no `chown` is needed. Use a new volume,
not a reused one.) Run this snippet from Bash or Zsh:

```bash
gitsync_dir="${XDG_CONFIG_HOME:-$HOME/.config}/mkonnect-gitsync"
# Pin by digest where the release has one: public.ecr.aws/h2a8k0r3/memento-gitsync@sha256:<digest>
gitsync_image=public.ecr.aws/h2a8k0r3/memento-gitsync:<release-id>

(
  umask 077
  mkdir -p "$gitsync_dir"
  cat > "$gitsync_dir/gitsync.env" <<'EOF'
GATEWAY_URL=<gateway-url-from-your-instructions>
CONNECTOR_ID=<git-sync-connector-id>
GIT_SYNC_REPLICA_HOST=<replica-host>
KEY_FILE=/data/gitsync-key
EOF
  # Used only for the first start; removed once the connector shows as connected.
  printf 'REGISTRATION_TOKEN_FILE=/run/secrets/registration-token\n' > "$gitsync_dir/registration.env"
)

# Store the token in a volume as a 0600 file owned by uid 65532.
docker volume create gitsync-secrets
printf 'Paste registration token: ' >&2
IFS= read -r -s GITSYNC_TOKEN
printf '\n' >&2
printf %s "$GITSYNC_TOKEN" | docker run --rm -i --entrypoint sh -v gitsync-secrets:/data "$gitsync_image" \
  -c 'umask 077; cat > /data/registration-token'
unset GITSYNC_TOKEN

docker volume create gitsync-data
docker volume create gitsync-tmp

docker run -d --name mkonnect-gitsync --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  -v gitsync-data:/data \
  -v gitsync-tmp:/tmp \
  -v gitsync-secrets:/run/secrets:ro \
  --env-file "$gitsync_dir/gitsync.env" \
  --env-file "$gitsync_dir/registration.env" \
  "$gitsync_image"
```

Notes:

- `gitsync-data` is where the private key (`/data/gitsync-key`), the provider configuration
  (`/data/git-sync.json`), the mirrors (`/data/mirrors`), and the run state
  (`/data/mirrors/run-state.json`, see [Data on the volume](#data-on-the-volume)) live. Removing it
  deletes all of them.
- To turn off the automatic removal of old mirrors, add `GIT_SYNC_MIRROR_GC=off` to `gitsync.env`
  and recreate the container (see [Mirror cleanup](#mirror-cleanup)).
- `gitsync-tmp` is scratch space only and can be recreated at any time. Docker does not cap the
  size of a volume, so make sure the host has room for it.
- Keep `gitsync-data`, `gitsync-tmp`, and `gitsync-secrets` separate from the API connector's
  `mkonnect-data` volume.
- `gitsync-secrets` is mounted read-only (`:ro`).
- If your git server is on the Docker host or a sibling container, the same networking rules as for
  the API connector apply; see [Reaching a tool from Docker](setup.md#reaching-a-tool-from-docker).
- To go through an HTTP proxy, see [Using an HTTP proxy](#using-an-http-proxy).
- `--read-only` makes the root file system read-only. Docker still provides a small in-memory
  `/dev/shm`, which these options do not manage.

Next, [configure your git server credential](#configure-your-git-server-credential), then
[verify the connection](#verify) and [remove the registration token](#remove-the-registration-token).

## Option B: Kubernetes (Helm)

git-sync is part of the `mkonnect` chart and is **off by default**. Enabling it adds a separate
Deployment, its own `PersistentVolumeClaim`, and a `NetworkPolicy`; it does not change the API
connector's resources. The commands below add git-sync to an existing release created as in the
[Installation guide](installation.md#option-b--kubernetes-helm). For a new release, also pass the
API connector's `--set` options from that guide.

`--reset-then-reuse-values` (Helm 3.14+) takes the new chart's defaults and then re-applies the
values you set on earlier installs and upgrades. The connector guide uses `--reuse-values`, which
is fine for the connector alone. For a release that has git-sync, use
`--reset-then-reuse-values` for every upgrade instead: on Helm 3, `--reuse-values` replaces the
chart's defaults with the previous release's values, so settings added by a newer chart (such as
the defaults for git-sync) would be missing.

> **Warning: always keep the git-sync values on an upgrade.** `helm upgrade` without
> `--reset-then-reuse-values` or a `-f` values file that repeats your `gitSync.*` settings resets
> `gitSync.enabled` to `false`. Helm then **deletes the git-sync Deployment and its
> `PersistentVolumeClaim`, which holds the connector's private key and all mirrors**, and the
> connector would have to be registered again with a new one-time token. The chart deliberately
> does not mark the claim to be kept, so that uninstalling really removes the key and the
> mirrors.

Quote values that look like numbers (for example `tag: "20261022.0"` or
`sizeLimit: "21474836480"`) in values files: an unquoted number is read as a floating-point number
and can come out in exponent form, and Helm also stores `--set` integers as floats in the release
history.

Create the registration token as a Secret from an owner-only file. Run this snippet from Bash or
Zsh:

```bash
gitsync_dir="${XDG_CONFIG_HOME:-$HOME/.config}/mkonnect-gitsync"
(
  umask 077
  mkdir -p "$gitsync_dir"
  : > "$gitsync_dir/registration.token"
)
printf 'Paste registration token: ' >&2
IFS= read -r -s GITSYNC_TOKEN
printf '\n' >&2
printf %s "$GITSYNC_TOKEN" > "$gitsync_dir/registration.token"
unset GITSYNC_TOKEN

kubectl create secret generic gitsync-registration-token \
  --from-file=registration-token="$gitsync_dir/registration.token"

helm upgrade --install mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --version <chart-version> --reset-then-reuse-values \
  --set gitSync.enabled=true \
  --set gitSync.image.repository=public.ecr.aws/h2a8k0r3/memento-gitsync \
  --set gitSync.image.digest=sha256:<git_sync_image_digest> \
  --set gitSync.config.connectorId=<git-sync-connector-id> \
  --set gitSync.config.replicaHost=<replica-host> \
  --set gitSync.config.registrationTokenSecret.name=gitsync-registration-token
```

If you cannot pin a digest, omit `gitSync.image.digest`: the image tag then defaults to the chart's
`appVersion`, which is only valid if that release has a git-sync image (see
[Check that the release has a git-sync image](#check-that-the-release-has-a-git-sync-image)).

The chart mounts the token read-only at `/etc/mkonnect-gitsync/registration-token/` (mode `0440`,
readable through the pod's `fsGroup`) and points `REGISTRATION_TOKEN_FILE` at it. Instead of a
Secret you manage, you can let the chart create one (named `<release>-gitsync-registration`) with
`--set-file gitSync.config.registrationToken="$gitsync_dir/registration.token"`. Either way the
token is delivered as a file; it is never an environment variable.

What the chart sets up:

| Value | Default | Meaning |
|---|---|---|
| `gitSync.enabled` | `false` | Turns the add-on on. When `false` nothing git-sync-related is rendered. |
| `gitSync.image.repository` | (required) | The git-sync image, e.g. `public.ecr.aws/h2a8k0r3/memento-gitsync`. |
| `gitSync.image.tag` | chart `appVersion` | The release ID. The chart does not guarantee that image exists; see above. |
| `gitSync.image.digest` | empty | Recommended. `sha256:` digest; when set, the image is `repository@digest` and the tag is ignored. |
| `gitSync.config.connectorId` | (required) | The git-sync connector's own ID. Must differ from `config.connectorId`. |
| `gitSync.config.replicaHost` | (required) | The replica host; host name, IP, or `[IPv6]`, optionally with `:port`. The chart rejects a scheme or a path. |
| `gitSync.config.gatewayUrl` | `config.gatewayUrl` | Override only if you were given a different gateway URL for git-sync. |
| `gitSync.config.registrationTokenSecret.name` / `.key` | empty / `registration-token` | An existing Secret holding the one-time token (preferred). |
| `gitSync.config.registrationToken` | empty | Alternative: the chart creates the Secret from this value (use `--set-file`). |
| `gitSync.providerConfig.secretName` / `.key` | empty / `git-sync.json` | An existing Secret holding the git server credentials; see below. |
| `gitSync.caBundle.secretName` / `.key` / `.mountPath` | empty / `ca-bundle.pem` / `/etc/mkonnect-gitsync-ca` | A PEM bundle for git to trust; see below. |
| `gitSync.persistence.size` / `.storageClass` | `20Gi` / cluster default | The git-sync volume. |
| `gitSync.tmp.sizeLimit` | `5Gi` | Size limit of the disk-backed `/tmp` scratch volume. |
| `gitSync.resources` | 250m / 512Mi requests, 1 / 2Gi limits | CPU and memory. |
| `gitSync.proxy.https` / `.http` / `.noProxy` | empty | Optional proxy; see [Using an HTTP proxy](#using-an-http-proxy). |
| `gitSync.networkPolicy.enabled` | `true` | Deny all ingress to git-sync pods; see the [security guide](git-sync-security.md#network-policy). |
| `gitSync.networkPolicy.egress.cidrs` / `.ports` | empty / `[443]` | Optional egress allow-list; empty leaves egress unrestricted. |

See [`charts/mkonnect/values.yaml`](../charts/mkonnect/values.yaml) for every option.

To remove a default rather than change it, such as the CPU limit, set it to `null`
(`--set gitSync.resources.limits.cpu=null`): Helm drops the key before the chart renders. Setting it
to an empty value does not remove it.

The pod runs as uid/gid `65532` with a read-only root filesystem, no capabilities, no privilege
escalation, the runtime-default seccomp profile, and no mounted ServiceAccount token. It exposes
no port and there is no Service. The Deployment has one replica and uses the `Recreate` strategy,
so an upgrade stops the old pod before the new one starts.

### Trusting an internal CA for git

If your git server's certificate is signed by an internal CA, put a PEM bundle in a Secret and
point the chart at it:

```bash
kubectl create secret generic mkonnect-gitsync-ca --from-file=ca-bundle.pem=<bundle-file>
```

```yaml
gitSync:
  caBundle:
    secretName: mkonnect-gitsync-ca
```

git then trusts **only** this bundle (`GIT_SSL_CAINFO`) for all of its connections, including the
push to the Memento replica. The bundle must therefore contain your internal CA **and** the public
root certificate(s) that sign the replica's certificate. With Docker, mount the bundle read-only
and set `GIT_SSL_CAINFO` to its path in `gitsync.env`. The CA bundle applies to git only, not to
the connector's own connection to the gateway.

### Using an HTTP proxy

git-sync reads the standard proxy variables in two places:

- **git** (every fetch, push, and reachability check). git-sync passes `HTTPS_PROXY`,
  `HTTP_PROXY`, `NO_PROXY` and their lower-case forms, and nothing else from its environment, on to
  git. git's HTTP library reads **only the lower-case `http_proxy`** for plain `http://` URLs and
  ignores upper-case `HTTP_PROXY` on purpose, so set both cases.
- **the connection to the gateway**, which uses Go's default HTTP transport. It reads
  `HTTPS_PROXY` (or `https_proxy`) for its `wss://` connection, and `NO_PROXY` (or `no_proxy`).

With Docker, add both cases of each variable to `gitsync.env`. With Helm, set
`gitSync.proxy.https`, `gitSync.proxy.http`, and `gitSync.proxy.noProxy`; the chart exports each
one in both upper and lower case. Do not put credentials in a proxy URL: values are visible in the
Deployment specification. The proxy must allow `CONNECT` tunnelling to the gateway, the replica,
and your git servers, and must pass the gateway's WebSocket upgrade. Add your cluster's internal
addresses to `NO_PROXY` if git servers are reached directly. This is based on reading the code and
the behaviour of git and Go; it has not been tested through every kind of proxy.

## Configure your git server credential

git-sync only uses a credential against a repository whose origin URL has the same scheme, host,
and port as a **provider** you have configured. Each provider has a name you choose, a base URL
(the scheme and host of your git server, such as `https://git.example.com`), and optionally a
credential. Each host may be configured once. A repository whose host is not configured is
reported as `origin_access_denied` and never contacted with a credential.

Token rules: at least 16 characters; given only on standard input (`--token`, which would be
visible in the process list and shell history, is rejected); an authentication mode requires an
`https://` base URL. A provider name is 1 to 64 letters, digits, `.`, `_`, or `-`.

**Docker.** Read the token without echoing it and pipe it in. Run from Bash or Zsh:

```bash
printf 'Paste git access token: ' >&2
IFS= read -r -s GIT_TOKEN
printf '\n' >&2

# Bearer token:
printf %s "$GIT_TOKEN" | docker exec -i mkonnect-gitsync /mkonnect-gitsync \
  connector config set internal-git --base-url https://git.example.com --auth bearer --token-stdin

# HTTP Basic (username and token/password):
printf %s "$GIT_TOKEN" | docker exec -i mkonnect-gitsync /mkonnect-gitsync \
  connector config set internal-git --base-url https://git.example.com \
  --auth basic --username <service-account> --token-stdin
unset GIT_TOKEN

docker exec -i mkonnect-gitsync /mkonnect-gitsync connector config list    # credentials shown masked
docker exec -i mkonnect-gitsync /mkonnect-gitsync connector status
```

The running connector picks the change up within a few seconds (see
[Reloading the provider configuration](#reloading-the-provider-configuration)); it needs no restart.

**Kubernetes.** The same commands work with `kubectl exec -i deploy/mkonnect-gitsync --
/mkonnect-gitsync connector ...` when you let git-sync manage the file in its volume
(`/data/git-sync.json`, owner-only). The recommended approach is to keep the credential in a Secret
you control (which can itself come from Vault or the External Secrets Operator) and mount it. The
creation step uses a short-lived local file, so create it with restrictive permissions outside your
checkout and remove it afterwards:

```bash
secret_dir="$(umask 077 && mktemp -d)"
providers_file="$secret_dir/git-sync.json"
(umask 077 && cat > "$providers_file" <<'EOF'
{
  "internal-git": {
    "base_url": "https://git.example.com",
    "auth": "bearer",
    "token": "REPLACE_WITH_ACCESS_TOKEN"
  }
}
EOF
)
# Replace the placeholder in a local editor; do not paste a real token into this shell block.
${EDITOR:-vi} "$providers_file"
kubectl create secret generic mkonnect-gitsync-providers \
  --from-file=git-sync.json="$providers_file" && rm -r "$secret_dir"

helm upgrade mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --version <chart-version> --reset-then-reuse-values \
  --set gitSync.providerConfig.secretName=mkonnect-gitsync-providers
```

Each entry has `base_url` (required), `auth` (`bearer`, `basic`, or omitted for none), `username`
(basic only), and the credential: either `token` or `token_file` (see
[Reading the credential from a file](#reading-the-credential-from-a-file-token_file)). Unknown
fields are rejected. The Secret is mounted read-only at
`/etc/mkonnect-gitsync-providers/` (mode `0440`, group-readable through the pod's `fsGroup`),
outside the data volume, so `connector config set` cannot modify it: change it by updating the
Secret. The mirrors stay on the data volume under `/data/mirrors`.

### Reading the credential from a file (`token_file`)

Instead of an inline `token`, a provider can name a file that holds the credential. Use it where
the credential is delivered as a file (a Docker volume or secret, a mounted Kubernetes Secret, a
secrets-manager agent) so it never has to be copied into the provider configuration:

```json
{
  "internal-git": {
    "base_url": "https://git.example.test",
    "auth": "bearer",
    "token_file": "/run/secrets/git/git-token"
  }
}
```

- A provider with an authentication mode has **exactly one** of `token` and `token_file`. Setting
  both, or neither, is an error, as is `token_file` without `auth`. The path must be absolute. A
  key counts as set even when its value is empty: `"token_file": ""` is an error, not a missing
  credential (only an absent key, or `null`, counts as not set).
- The file holds the credential and nothing else. **One trailing newline (`\n`) is removed; nothing
  else is trimmed.** An empty file, a Windows line ending (`\r\n`), a second line or blank line,
  leading or trailing spaces, control or invisible characters (including a byte-order mark), and
  text that is not valid UTF-8 are all refused, rather than quietly changing the credential that is
  sent. The size limit is 4096 bytes, and the usual token rules (at least 16 characters) apply.
- The file is read when the provider configuration is loaded: at startup, and again on every
  [reload](#reloading-the-provider-configuration), which a change to the token file (or to the
  provider configuration) triggers. Rotating the credential means overwriting the file; git-sync
  stops syncs still using the old one and syncs again with the new. A symbolic link is followed (a Kubernetes Secret volume uses one);
  the checks below apply to the file it leads to.
- `connector config list` shows such a provider as `bearer *** (token_file)`, without the path.
  `connector config set` cannot create one: write the file. Running `config set` for a *different*
  provider keeps the `token_file` entry as it is. Running it for the same provider replaces the
  entry with an inline token.
- No message or log line contains the credential or any of the file's content; errors name the
  provider and what is wrong with the file.

What git-sync checks on each file before using it:

| Condition | Token file (`token_file`) | Provider configuration file |
|---|---|---|
| Not a regular file, or too large | Refused | Refused |
| Writable by its group or by others | **Refused** | Tightened to `0600`. If that is not possible (a read-only mount, or a file another user owns, which git-sync never changes), refused unless the mode already denies all access to others and any write by the group |
| Readable by its group or by others | **Warning, once per file**: the file is never changed. Docker secrets default to `0444`, and a read-only Kubernetes Secret mount cannot be tightened, so refusing would make `token_file` unusable there | Tightened to `0600`; on a read-only mount accepted when only the group can read |
| Owner is neither the user running git-sync nor `root` | **Refused** | Refused if the file names a `token_file`. Otherwise only a **warning in this release**; a later release will refuse to start. Make the user running git-sync (uid 65532 in the image) or `root` the owner now |
| Directory lets other users rename files in it | Warning, once per file. Someone who can write there can swap the file, or a symbolic link to it, for another file. Only the directory of the path as configured is checked, not where a symbolic link leads | Warning, once per file |

The provider configuration gets the stricter owner check when it uses `token_file` because it then
decides which local file is sent to which git server: someone who could edit it could point a
provider at any file the connector can read.

Where this matters:

- **Docker.** Keep the credential in a volume, as for the registration token, so that the file is
  owned by uid 65532 and `0600`. A file bind-mounted from the host, including a Compose file-based
  secret, keeps the host file's owner and is refused unless that owner is `root` or uid 65532.
  Swarm secrets are `root`-owned and `0444`, so they work and log the read warning.
- **Kubernetes.** Secret files are `root`-owned and readable by the pod user through `fsGroup`,
  so the read warning on every start is expected and harmless there. The Helm chart does not yet
  mount an extra Secret for this, so the ready-made setup is the provider configuration Secret
  above; use `token_file` with a volume you add to the Deployment yourself.

An example for Docker. It stores the credential in its own volume (not `gitsync-secrets`, which
you delete after registration), writes a provider configuration that points at it, and mounts the
volume read-only. Run it from Bash or Zsh, with `$gitsync_image` set as in
[Option A](#option-a-docker):

```bash
docker volume create gitsync-git-credential
printf 'Paste git access token: ' >&2
IFS= read -r -s GIT_TOKEN
printf '\n' >&2
printf %s "$GIT_TOKEN" | docker run --rm -i --entrypoint sh -v gitsync-git-credential:/data "$gitsync_image" \
  -c 'umask 077; cat > /data/git-token'
unset GIT_TOKEN

# This replaces the whole file: if /data/git-sync.json already holds providers, include them here.
docker run --rm -i --entrypoint sh -v gitsync-data:/data "$gitsync_image" \
  -c 'umask 077; cat > /data/git-sync.json' <<'JSON'
{
  "internal-git": {
    "base_url": "https://git.example.test",
    "auth": "bearer",
    "token_file": "/run/secrets/git/git-token"
  }
}
JSON
```

Then add `-v gitsync-git-credential:/run/secrets/git:ro` to the `docker run` command that starts
git-sync and restart it. To rotate, overwrite `/data/git-token` in the volume the same way;
git-sync picks it up within seconds, without a restart.

**Check the file before you rely on it.** `connector config check` loads the provider configuration
the way git-sync does at startup and reports every problem with the providers at once, instead of
the first one the connector would stop at. (A problem with the file as a whole, such as unreadable,
not JSON, or an unknown field, stops the check just as it stops the connector, so it is reported
alone.) It contacts no server and never prints a credential, or any value from
the file: for each valid provider it shows the name, the canonical host key a repository's origin
URL must resolve to, the authentication mode, and where the credential comes from (`inline
token`, stored in the file, or `token_file`).

```bash
docker exec -i mkonnect-gitsync /mkonnect-gitsync connector config check
```

```text
=== Providers ===
  NAME      HOST KEY                      AUTH    CREDENTIAL
  internal  https://git.example.com:443   bearer  inline token

=== Errors ===
  [credentials] provider "other" (field token): token must be at least 16 characters
```

The check also reads every `token_file` and applies the
[checks above](#reading-the-credential-from-a-file-token_file) to it, so it reports a file that is
missing, unreadable, or has the wrong owner or mode, and a malformed token, as problems of the
provider under `credentials` (field `token_file`). A configuration that has any other problem opens
no token file, so those are reported once the rest is fixed. A token file that group or others can
read is not an error and is logged as a warning, as at startup. Because the check reads these files
as the user running it, run it as the user git-sync runs as (for example with `docker exec`, without
`-u 0`, which would see files owned by uid 65532 as foreign).

Each error starts with a class: `file` (cannot be read, is not a regular file, is too large, or has
unsafe permissions), `syntax` (not one JSON value), `schema` (an unknown field, or a setting that is
not a string), `provider_name`, `base_url`, `credentials`, or `duplicate_host`, followed by the
provider and the field at fault. A provider name or field that is not valid is described by its
position or length, never repeated. The command exits non-zero if the connector would refuse the
file. It checks the file named by `GIT_SYNC_CONFIG_FILE`, else `/data/git-sync.json` (there is no
path option, so it cannot read a different file from the one git-sync reads); a missing file at the
default path (`/data/git-sync.json`) is only reported, since git-sync then starts with no providers,
but a missing file at any other path is an error. Run it as the user git-sync runs as and with the same
environment: like git-sync, it tightens the file to owner-only (`0600`) where it is allowed to, and
it warns when the file was wider than that.

### Reloading the provider configuration

git-sync watches the provider configuration while it runs, so adding a provider, rotating a
credential, or revoking one takes effect without a restart, within a few seconds of the file
changing.

- **How it notices.** Every 5 seconds git-sync reads the provider file, and each token file it
  names, and compares them with what it loaded. A change is applied only after it has been seen twice, one second apart, so a file that is
  still being written, or that is saved in several steps, is not applied half-way, and a file that
  changes and changes back is not applied at all. On Kubernetes this covers a Secret mounted as a
  volume, which the kubelet updates in place; the kubelet delivers an updated Secret to the pod
  only after a delay (often a minute or more), and nothing git-sync does can shorten it.
- **Apply it now.** Send `SIGHUP` to the process to skip git-sync's own wait (it re-reads the file
  at once; it cannot make the file arrive sooner):

  ```bash
  docker kill --signal=HUP mkonnect-gitsync                 # Docker
  kubectl exec deploy/mkonnect-gitsync -- kill -HUP 1       # Kubernetes
  ```

  `SIGHUP` used to end the process; it now means "reload". The log says what happened either way.
- **A provider is removed, or its credential changes.** A sync that is using it is stopped at
  once (its git processes are killed) and what it had done is discarded: its result is never
  reported or remembered, and the cancelled sync pushes nothing. (What it had already fetched stays in
  the mirror and is pushed by the next sync.) If the credential changed,
  the repository is synced again straight away with the new one. Syncs that use other providers
  are not touched, and a change that leaves a provider as it was does nothing. A rotation that
  happens more often than your longest sync takes starves that sync, because every rotation
  restarts it.
- **A provider is added.** Repositories that were reported as `origin_access_denied` with
  `host not configured locally` start syncing; you do not need Memento to resend anything.
  Repositories whose provider was removed report `origin_access_denied` with that same detail.
- **The configuration cannot be used** (the provider file is not valid, a value is rejected, it is
  unreadable, its mode is too open and cannot be tightened, or a token file exists but cannot be
  used, for example because it is empty while being rewritten; a mode git-sync can tighten, as at
  startup, is simply tightened). git-sync keeps using the last configuration that worked, for as
  long as it takes you to fix it, and logs one line saying that it cannot be used, with the kind of
  problem (`file`, `syntax`, `schema`, `provider_name`, `base_url`, `credentials` or
  `duplicate_host`), once for each different content (again if the files go back to a working
  state and then to the same bad content). It never logs anything taken from the files. Run
  `connector config check` for the details. This is not reported to Memento; check the log after an
  edit you expect to take effect.
- **The provider file is deleted.** After the same short wait git-sync treats the configuration as
  empty: every provider credential is revoked, running syncs are stopped, and every repository
  reports `origin_access_denied` until the file is back. The log says so as an error. A mount that
  briefly removes the file while it is replaced is covered by the wait.
- **A token file is deleted.** After the wait, the provider that read its credential from it is
  dropped, like a removed provider (its syncs are stopped and its repositories are denied), and the
  other providers carry on. The log says so as an error. If the file comes back, so does the provider.
  A token file that is merely unreadable or unusable does not do this (see above).
- **Only the provider configuration is reloaded.** The replica host pin (`GIT_SYNC_REPLICA_HOST`),
  the registration token file, the signing key file and every other setting are read once at
  startup, and changing them needs a restart:

  ```bash
  docker restart mkonnect-gitsync                           # Docker
  kubectl rollout restart deployment/mkonnect-gitsync       # Kubernetes
  ```

While syncing is suspended for a critical update, a rotation still stops the syncs that use the old
credential, but the repository is next synced when the suspension ends and its schedule falls due.

Each reload that changes something re-runs the check that the configured git servers are reachable.
`connector status` lists the configured providers and the replica host the process sees in its
environment; the state of enrollment and of each repository is shown in the Memento portal.

## Verify

git-sync should register and connect within a few seconds.

- **Portal:** the git-sync connector shows as connected, with a recent heartbeat. Once a provider
  is configured, the portal also shows whether your git servers are reachable and accept the
  credential (one combined result, the worst across the servers you configured: `reachable`,
  `access_denied`, or `unreachable`), before any repository is added. git-sync re-checks it about
  every 30 seconds until the platform's first configuration is accepted and about every 5 minutes
  afterwards, and keeps reporting it once repositories are syncing, so a credential that is later
  revoked shows up there too. The result is absent while no provider is configured, until the
  first check has finished, and when the check cannot run at all (for example git is missing from
  the image; the log says `origin reachability check could not run`). After you add repositories,
  each shows its sync state, when it last succeeded, and the outcome of its last attempt (see
  [Run state](#run-state)).
- **Docker logs:**

  ```bash
  docker logs mkonnect-gitsync
  ```

  A healthy start logs `mkonnect-gitsync starting` with the connector ID, the gateway, and the
  number of configured providers, and `replica host pinned`. When the platform sends repositories
  you will see `git_sync_config accepted`, then `sync complete` (or `sync failed` with an error
  type) per repository.
- **Kubernetes:** `kubectl logs deploy/mkonnect-gitsync` and
  `kubectl get pods -l app.kubernetes.io/name=mkonnect-gitsync`.

The Kubernetes startup probe only checks that the binary starts. It says nothing about the
configuration: a pod can be `Running` and `Ready` while git-sync is not syncing (for example
because its configuration was rejected). Always check the logs and the portal.

If a repository shows an error, see [Troubleshooting](#troubleshooting).

## Remove the registration token

Only remove the registration token after the portal shows the connector as **connected**. Until
then keep the token (the `gitsync-secrets` volume, or the Kubernetes Secret) so a failed first
start can be retried.

**Docker:** recreate the container without the token volume and without `registration.env`, then
delete the volume. The two go together: if `REGISTRATION_TOKEN_FILE` is set, the file must exist
and be readable only by its owner and group, so either keep both or remove both. The key is on
`gitsync-data`, so the new container reconnects without registering again:

```bash
gitsync_dir="${XDG_CONFIG_HOME:-$HOME/.config}/mkonnect-gitsync"
gitsync_image=public.ecr.aws/h2a8k0r3/memento-gitsync:<release-id>   # or @sha256:<digest>
rm "$gitsync_dir/registration.env"
docker rm -f mkonnect-gitsync
docker run -d --name mkonnect-gitsync --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  -v gitsync-data:/data \
  -v gitsync-tmp:/tmp \
  --env-file "$gitsync_dir/gitsync.env" \
  "$gitsync_image"
docker volume rm gitsync-secrets
```

**Kubernetes:** stop mounting it and delete the Secret:

```bash
gitsync_dir="${XDG_CONFIG_HOME:-$HOME/.config}/mkonnect-gitsync"
rm "$gitsync_dir/registration.token"
helm upgrade mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --version <chart-version> --reset-then-reuse-values \
  --set gitSync.config.registrationTokenSecret.name="" \
  --set gitSync.config.registrationToken=""
kubectl delete secret gitsync-registration-token
```

If you passed the token with `--set-file`, earlier Helm release revisions still contain it; see the
note in the [Installation guide](installation.md#remove-the-registration-token-after-verification).
Using an existing Secret, as above, keeps the token out of Helm release history entirely.

## Sync schedule and retries

The platform sends no timer, so git-sync re-syncs every repository on its own schedule.

- **Interval.** A repository is synced again 15 minutes (the default interval) after its previous
  sync *finished*, give or take 10%, so repositories do not all fall due together. The first
  scheduled run of a newly configured repository (or one whose origin or replica changed, and of
  every repository after a restart) is spread out further: the interval plus a random delay of up
  to 2 minutes (at most the interval itself). git-sync looks for due repositories every 5 seconds,
  so a sync can start a few seconds late.
- **No pile-up.** A scheduled run is skipped for a repository that is still syncing (queued or
  running), and it is not made up afterwards. Because the interval is measured from the end of the
  previous sync, a slow repository is never synced back to back.
- **Immediate syncs.** An accepted configuration (the platform may re-send it, for example to renew
  push tokens) and an immediate-sync request from Memento both start a sync at once, whatever the
  schedule or the backoff below says. If the repository is already syncing, one more sync follows
  when it ends, however many requests arrived.
- **The platform can change the interval.** The configuration it sends may carry an interval from
  60 seconds to 24 hours; one that omits it gets the 15-minute default. A new interval applies
  without a restart. A value outside the range rejects the whole configuration with
  `limits_out_of_range` (see [Rejected configuration](#rejected-configuration-config_error)).
- **Only repositories that can be synced are scheduled.** A repository whose host has no
  provider on your side (`origin_access_denied` with `host not configured locally`), or whose
  replica configuration is unusable, is reported but not synced at all until that changes (for
  the first, add the provider (git-sync picks it up without a restart); for the second, the platform has to send a
  corrected configuration).

### Concurrency

By default up to four repositories sync at once, and the others wait for a free slot. A repository
that is waiting is reported as `syncing`, so with many repositories one can show `syncing` for a while
before git actually starts on it. Memento can change the number (see
[Limits the platform can send](#limits-the-platform-can-send)); a change takes effect without a
restart and never interrupts a sync that is already running.

### Failures and backoff

A failed sync is retried with a growing delay, so a persistent fault does not hammer your git
server or the replica. After *n* consecutive failures the next scheduled attempt waits
`interval × 2^(n−1)`, but never less than the interval and never more than one hour (or the interval, if that is
longer), plus a random
extra of up to 20% (a retry is never made sooner than that wait). With the default 15-minute
interval:

| Consecutive failures | Next scheduled attempt after |
|---|---|
| 1 | 15 to 18 minutes |
| 2 | 30 to 36 minutes |
| 3 or more | 60 to 72 minutes |

If the interval is longer than one hour, failures are retried at that interval (plus the extra).

Three failure types wait the longest delay straight away, from the **first** failure:
`origin_access_denied`, `credential_rejected`, and `push_token_expired` (at least one hour, or the
interval if that is longer, plus the extra). Retrying sooner cannot succeed until a credential or the
configuration changes, and repeated failed logins can lock a service account out.

A successful sync clears the backoff. A repository whose origin or replica changes in a new
configuration starts again with a fresh schedule; a configuration that leaves them unchanged (for
example a push-token refresh) keeps its backoff, though it still starts one attempt at once, and
that attempt's result counts as usual. The backoff is held in memory only: restarting git-sync
resets it, and after a restart repositories are synced when the platform next sends the
configuration. A sync cut short by shutdown counts as neither a success nor a failure.

To retry sooner after you fix a cause, ask Memento for an immediate sync or restart git-sync. A
credential you change in the provider configuration needs neither: git-sync syncs the repositories
that use it again as soon as it has loaded the change (see
[Reloading the provider configuration](#reloading-the-provider-configuration)).

### While syncing is suspended

If the gateway reports that a critical security update is required, git-sync starts no new sync,
neither scheduled nor requested (a sync already running is not cancelled), and keeps the results
it already has; repositories that have not synced yet stay `pending`. Its status carries
`suspended: critical_update_required` (how the portal shows it is up to the platform), and the log
says `gateway requires a critical security update`. Mirror cleanup is paused too. Upgrade to the
latest release. git-sync reads the flag again on every connection, so syncing resumes by itself
once the gateway no longer requires an update. When it resumes, repositories that fell due in the
meantime start spread over up to 2 minutes (less if the interval is shorter), not all at once.

### Limits the platform can send

The configuration may also carry resource limits. git-sync checks every value against its accepted
range and rejects the whole configuration if one is outside it (see
[Rejected configuration](#rejected-configuration-config_error)). A limit the configuration leaves out
goes back to its default. These are applied:

| Limit | Default | Effect |
|---|---|---|
| Interval | 15 minutes | How often each repository is synced. |
| Concurrency | 4 | How many repositories sync at once. Changing it never interrupts a running sync: raising it starts waiting repositories at once, lowering it only holds back new starts until enough syncs finish. |
| Push timeout | 30 minutes | How long one push to the replica may run before it is reported as `push_timeout`. The fetch from your git server keeps its fixed 30-minute limit. |
| Pack threads, pack window memory | git's own | Passed to git as `pack.threads` and `pack.windowMemory`, for the fetch and the push, to bound CPU and memory on a small host. |
| Repository size ceiling | none | After a fetch, a mirror larger than the ceiling is not pushed and the repository is reported as `repository_over_limit`. The check is on the mirror's size on disk, after the fetch: a repository far over the ceiling is still downloaded once, and its mirror is kept so the next fetch is incremental. The error repeats until git has reclaimed enough space or the ceiling is raised. |
| Push batch size | none | A push that would write more refs than this is made in batches of at most that many refs, so a very large repository is not sent in one request (see below). |

When pushes are batched, git-sync first asks the replica what a push would change (a request that
sends no data), writes the changed branches and tags in batches, and only after **every batch has
succeeded** makes one final push that also removes the refs that no longer exist on your git server.
If a batch fails, the token expires, or git-sync is stopped before that final push, nothing is
removed from the replica in that run and the next run continues from where the replica stands. The
batch size bounds the refs written per batch; the final push carries all the removals, and also
writes the few refs that cannot go in a batch (for example a name that git would find ambiguous next
to another ref). A repository whose mirror holds no more refs than one batch is pushed as before,
without the extra request.

## Data on the volume

Everything git-sync keeps lives on the data volume (`/data`). Each configured repository has one
bare mirror, and one small file records what happened to each repository.

| Path | Contents |
|---|---|
| `/data/gitsync-key` | The private key issued at registration. |
| `/data/git-sync.json` | The provider configuration, unless it is mounted from a Secret. |
| `/data/mirrors/<64 hex characters>.git` | The bare mirror of one repository. |
| `/data/mirrors/run-state.json` | The [run state](#run-state). |
| `/data/mirrors/.mkonnect-gc-trash/` | Created the first time a mirror is removed and kept afterwards, normally empty; a mirror sits here only briefly while it is being deleted (see [Mirror cleanup](#mirror-cleanup)). |

The mirrors directory is `GIT_SYNC_MIRRORS_DIR` when that is set (the chart sets it to
`/data/mirrors`), otherwise a `mirrors` directory next to the provider configuration file.

### Mirror layout

A mirror's directory name is the SHA-256 of the repository ID, in lower-case hex, followed by
`.git`; the name never contains the repository ID itself. To find the mirror of a repository ID
(the IDs appear in the portal and in the logs), run:

```bash
printf %s '<repository-id>' | sha256sum    # on macOS: shasum -a 256
```

A sync fetches only branches and tags into the mirror. Do not edit or add files in the mirrors
directory: git-sync treats a directory named like a mirror as its own. If a mirror is damaged (git reports it as
corrupt or not a repository), the next sync deletes and rebuilds it from the origin. Deleting a
mirror yourself while git-sync is stopped is safe for the same reason: the next sync downloads the
repository again in full.

### Run state

`run-state.json` is a small JSON file that remembers, for each repository, the outcome of its last
attempts, so a restart does not make a healthy repository look as if it never synced. For each
repository it holds:

- when the last attempt started, how long it took, and how many refs it created, updated or
  deleted on the replica (zero is normal when nothing changed);
- `last_success_at`, when the last successful sync finished. A sync that found nothing new counts
  as a success. It is carried unchanged through failed attempts, and stays **empty until the first
  success** (absent from the file, and reported as `null` in the status);
- the committer time of the newest commit across the origin's branches and tags, and the origin's
  default branch, as last learned (a failed attempt leaves the previous values);
- a hash that identifies the origin and replica the record was made against.

It holds no credential, URL, host name, path, or error text. The file is owner-only (`0600`),
written atomically, and written within a few seconds of a sync finishing; a clean shutdown writes
it out. After a restart, once the platform has sent the configuration, a repository shows `syncing`
(or `pending` while syncing is suspended) until its first sync since the restart finishes, but it
reports its earlier last-success time, duration and ref count right away.

A repository's record is dropped when its origin or replica changes, because the old success says
nothing about the new replica, and when the repository is no longer in the configuration (unless the
configuration lists no repositories at all, which is treated as a platform mishap, not a decision).

git-sync starts and syncs normally whatever the state of this file. It ignores the whole file, and
logs `ignoring run state file` with a reason, when it is unreadable, not a regular file,
writable by everyone, larger than 8 MiB, not valid JSON, from an unsupported version, or listing
more than 4096 repositories (the reasons are `unreadable`, `not_regular`, `world_writable`,
`oversized`, `corrupt`, `unsupported_version`, and `too_many_entries`). It drops and counts individual invalid entries (`dropped invalid run state
entries`). The effect is only that last-success times start from empty (null) until the next
success. To reset them yourself, stop git-sync and delete the file.

A failure to write it is logged as `run state write failed` with the step that failed, and never
fails a sync. The usual cause is a full or read-only volume.

### Mirror cleanup

When a repository leaves the platform's configuration, its mirror would otherwise stay on the
volume for good. git-sync removes it after a grace period:

1. A mirror counts as orphaned when no repository in the **currently accepted** configuration maps
   to it. A repository the platform still lists keeps its mirror even if its host is not
   configured on your side or it is failing.
2. The first time git-sync sees a mirror orphaned it writes a `.mkonnect-orphaned` file in the
   mirror with the time, and logs `mirror is no longer in the configuration; it will be removed
   after the grace period`. The mark survives restarts. If the repository returns to the
   configuration before the grace period ends, the mark is removed and the grace period starts
   over the next time.
3. Once the mirror has been orphaned for **seven days** it is moved into
   `.mkonnect-gc-trash/` and deleted, and git-sync logs `removed orphaned mirror`. A removal that
   fails or is interrupted is retried at the next sweep.

Sweeps run after every accepted configuration and every 6 hours, so a mirror is removed within about
6 hours of its seven days ending. They are deliberately cautious. Nothing is removed:

- while `GIT_SYNC_MIRROR_GC=off` is set (the value is not case-sensitive; any other value, or none,
  leaves cleanup on). git-sync logs `mirror cleanup is disabled` at start;
- before the platform has sent a configuration since git-sync started, and while the accepted
  configuration lists **no repositories**. Removing the last repository in Memento therefore
  does not free its mirror by itself; delete it yourself with git-sync stopped if you want the
  space back;
- while the latest configuration was rejected (see
  [Rejected configuration](#rejected-configuration-config_error)) or a critical update is required;
- while a sync of that mirror is queued or running;
- while the system clock reads before 1 January 2025, because a mark written with a wrong clock
  would look ancient later (git-sync logs `mirror cleanup skipped`). The grace period relies on a
  correct host clock.

Only directories named exactly like a mirror (64 lower-case hex characters and `.git`) are ever
removed; symbolic links, the run-state file and every other entry are left alone.

With Docker, add `GIT_SYNC_MIRROR_GC=off` to `gitsync.env` and recreate the container. The Helm
chart has no value for it yet. `kubectl -n <namespace> set env deployment/mkonnect-gitsync GIT_SYNC_MIRROR_GC=off`
sets it on the running Deployment; check that it is still there after each `helm upgrade`, because
Helm manages that Deployment.

## Upgrading

Releases use calendar identifiers (`YYYYMMDD.n`). Always pin an explicit release, and check first
that the release has a git-sync image (see
[Check that the release has a git-sync image](#check-that-the-release-has-a-git-sync-image)).

**Docker:** pull the new image and recreate the container with the same volumes and env file:

```bash
gitsync_dir="${XDG_CONFIG_HOME:-$HOME/.config}/mkonnect-gitsync"
gitsync_image=public.ecr.aws/h2a8k0r3/memento-gitsync:<new-release-id>   # or @sha256:<digest>
docker pull "$gitsync_image"
docker rm -f mkonnect-gitsync
docker run -d --name mkonnect-gitsync --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  -v gitsync-data:/data \
  -v gitsync-tmp:/tmp \
  --env-file "$gitsync_dir/gitsync.env" \
  "$gitsync_image"
```

**Helm:** upgrade the chart and, if you pinned a digest, set the new one:

```bash
helm upgrade mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --version <new-chart-version> --reset-then-reuse-values \
  --set gitSync.image.digest=sha256:<new-git_sync_image_digest>
```

Without a pinned digest, the image tag follows the chart's `appVersion`, so the upgrade fails to
pull if that release has no git-sync image. If you pinned `gitSync.image.tag`, update that instead.

> **Warning:** keep `--reset-then-reuse-values` (or repeat all your `gitSync.*` values with `-f`)
> on every upgrade. A plain `helm upgrade` resets `gitSync.enabled` to `false`, which deletes the
> git-sync Deployment **and its volume with the private key and all mirrors**; you would then have
> to register the connector again with a new token. See [Uninstalling](#uninstalling).

> **Downgrading:** a release older than the one that introduced `token_file` rejects the field as
> unknown and will not start. Before you downgrade, replace each `token_file` entry with an inline
> `token` (for example with `connector config set`) or restore a configuration that has none.

The key, provider configuration, mirrors, and run state are on the volume, so the connector
reconnects with its existing key and syncs incrementally. A sync that is running when the container
stops is cancelled; the repository is synced again once the platform sends its configuration to the
restarted connector. The failure backoff is not kept across a restart (see
[Failures and backoff](#failures-and-backoff)). If the gateway reports that a critical security
update is required, git-sync starts no new sync until you upgrade (see
[While syncing is suspended](#while-syncing-is-suspended)).

## Uninstalling

**Docker:**

```bash
docker rm -f mkonnect-gitsync
docker volume rm gitsync-data gitsync-tmp gitsync-secrets   # deletes the private key, provider config, and all mirrors
```

**Helm:**

```bash
helm upgrade mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --version <chart-version> --reset-then-reuse-values --set gitSync.enabled=false
```

> **Warning: this deletes the git-sync data.** Disabling git-sync (and a plain `helm upgrade` that
> drops the `gitSync.*` values) removes the Deployment, ServiceAccount, NetworkPolicy, and the
> `PersistentVolumeClaim` (`mkonnect-gitsync-data`) with the private key and every mirror. The
> connector would need to be registered again with a new token to come back. The same happens with
> `helm uninstall`. The chart does not keep the claim, so removing git-sync never leaves a
> forgotten copy of your source code behind.

This leaves the API connector untouched. Your storage class's reclaim
policy decides whether the underlying disk is deleted; check with `kubectl get pv`. Secrets you
created yourself (the registration token, provider config, and CA bundle) are not removed;
delete them too. Deleting the volume deletes the private key and every mirror, which hold a full
copy of the replicated repositories, so treat disposal of the underlying disk accordingly.

What else to clean up:

- **Your git server:** git-sync only ever reads from it, so nothing it wrote needs reverting.
  Revoke the access token (or disable the service account) you created for git-sync.
- **Memento:** remove the git-sync connector, and the repositories that use it, in the Memento
  portal so that the platform stops expecting it. git-sync pushed branches and tags to the
  replica; stopping or removing the connector does not itself delete what was already replicated.
  The portal's repository removal controls what is kept on the Memento side.

## Troubleshooting

Per-repository errors appear in the portal with one of the types below, and the same type is
logged as `error_type` on a `sync failed` line. Details that could identify your infrastructure
(URLs, host names, paths) are deliberately left out of what is reported to the platform; look at
your own logs and network for those.

A failed sync is retried automatically, with a growing delay (see
[Failures and backoff](#failures-and-backoff)); you do not need to restart git-sync to make it try
again. The exceptions are the two kinds of repository that are never scheduled: one whose host is
not configured locally (adding the provider is picked up without a restart and syncs it at once), and
one with an unusable replica configuration. After you fix a cause, an immediate sync from Memento, or a restart (once the platform has
sent the configuration again), retries at once instead of waiting out the delay. A later success
clears the error.

| Symptom or error type | Likely cause and fix |
|---|---|
| `origin_access_denied` | Your git server rejected the credential (401, 403), or the repository was not found (a 404; many servers answer 404 for a repository the credential cannot see). Check the token's permissions and expiry; a credential you change in the provider configuration is picked up without a restart, and the repositories that use it are synced again at once. Scheduled retries wait at least an hour (to avoid piling up failed logins), but a configuration refresh from the platform or an immediate-sync request still makes one attempt at once. It is also reported, with the detail `host not configured locally`, when no provider matches the repository's scheme, host, and port: run `connector config list` and add the provider for that exact host. Such a repository is not retried at all until you add the provider, which git-sync picks up without a restart. |
| `origin_unreachable` | git-sync could not connect to your git server: DNS, firewall, proxy, a TLS certificate it does not trust (see [Trusting an internal CA](#trusting-an-internal-ca-for-git)), a server error (5xx), or a fetch that ran past its 30-minute limit (the repacking git does after a fetch runs inside that limit, so a very large repository can reach it that way). Test from the host that runs git-sync. |
| `replica_unreachable` | git-sync could not connect to the Memento replica. Allow outbound HTTPS to the replica host (and port) from the installation instructions. If you set a CA bundle for git, it must also contain the public root(s) for the replica. |
| `credential_rejected` | The **replica** rejected the push token (not your git credential). Tokens are issued by the platform and refreshed while the connector is connected. If it persists, check that the connector is connected and its host clock is correct, then contact Memento. Scheduled retries wait at least an hour; a configuration refresh or an immediate sync still tries once at once. |
| `push_token_expired` | The push token was past its expiry when a push was due. Usually the host clock is wrong, or the connector was disconnected and could not receive a refreshed token. Check NTP and the connection. Scheduled retries wait at least an hour; a configuration refresh or an immediate sync still tries once at once. |
| `replica_rejected` | The replica refused the push: a rejected ref, a policy decision, or a size or quota limit. Contact Memento with the repository ID. |
| `push_timeout` | A push ran past its time limit (30 minutes unless Memento set another), typically a very large first push over a slow link. Check bandwidth to the replica; later syncs push less. |
| `repository_over_limit` | The repository's mirror is larger than the size ceiling Memento configured, so nothing was pushed. Raising the ceiling is a Memento-side setting; contact Memento with the repository ID. The mirror stays on the data volume. |
| `local_disk_exhausted` | The data volume or the `/tmp` scratch space is full. The mirrors are on the data volume: grow `gitSync.persistence.size` (or the Docker volume's backing disk). For `/tmp`, raise `gitSync.tmp.sizeLimit`. See [Sizing](#sizing). |
| `job_out_of_memory` | git was killed for lack of memory. Raise the memory limit (`gitSync.resources.limits.memory`, or `--memory` with Docker). |
| `unclassified` | A failure that does not fit the types above. The report carries a short, scrubbed detail line; `docker logs` / `kubectl logs` have the same. Text a server sent in its own error (git's `remote error:` and `remote:` lines) is replaced by a fixed note, and any credential git-sync held for the sync is removed from git's output before it is read. The detail `origin has no branches or tags; replica left unchanged` means the origin is empty or advertised no branches or tags; git-sync deliberately does not touch the replica in that case. |
| The status carries a `config_error` (how the portal shows it is up to the platform) | The platform sent a configuration that git-sync rejected as a whole, and the previous configuration stays in effect. See [Rejected configuration](#rejected-configuration-config_error) for each reason, including the two replica-host pin errors, `replica_host_not_pinned` and `replica_host_mismatch`, that you can fix yourself. |
| The status carries `suspended: critical_update_required`, and the log says `gateway requires a critical security update` | git-sync starts no new sync until it is upgraded. Upgrade to the latest release; see [While syncing is suspended](#while-syncing-is-suspended). |
| A repository stays `syncing` for a long time | The first sync of a large repository is slow, or other repositories are ahead of it (four sync at a time by default, and a repository waiting for a slot is also reported as `syncing`). Watch `kubectl logs` / `docker logs` for the outcome. |
| A repository is `error` and was not retried for a long time | Failures back off, up to an hour or more, and the three credential failures wait at least an hour from the first one. See [Failures and backoff](#failures-and-backoff). |
| The last success time is empty (null) for a repository that used to sync | Either it has never succeeded since its origin or replica last changed, or the run state was discarded. Look for `ignoring run state file` in the log; see [Run state](#run-state). It fills in again after the next successful sync. |
| Disk usage does not drop after a repository was removed in Memento | Mirrors are removed seven days after the repository left the configuration, and not at all while cleanup is off, a configuration was rejected, or no repository is configured. See [Mirror cleanup](#mirror-cleanup). |
| Log warns `mirror cleanup skipped: the system clock reads an implausibly early time` | The host clock is before 2025. Fix the clock (NTP); cleanup resumes by itself. |
| Container exits at start with `configuration error: GATEWAY_URL …` or `CONNECTOR_ID is required` | `GATEWAY_URL` must be an absolute `wss://` URL, and `CONNECTOR_ID` must be set. |
| Container exits at start with `configuration error: REGISTRATION_TOKEN_FILE: …` | The registration token file could not be used. The message says which: `must be an absolute path`; `opening <path>: …` (the file is missing or unreadable: the volume or Secret is not mounted, or the Secret or key name is wrong); `<path> is not a regular file`; `<path> has unsafe permissions` (accessible by others or writable by group: use mode `0600`, `0640`, `0440`, or `0400`; in Docker re-create it as described in [Option A](#option-a-docker)); `<path> is larger than 4096 bytes` (you pointed it at the wrong file); `<path> is empty`. After registration, either keep the file or remove `REGISTRATION_TOKEN_FILE` together with it. |
| Start fails with `local config load failed` | Run `connector config check` to see every problem at once. The provider configuration is invalid: not valid JSON, an unknown field, a token shorter than 16 characters, an authentication mode on a non-`https` URL, two providers for one host, or a configuration file with unsafe permissions that git-sync cannot tighten (a read-only mount). The message names the cause and, for a token file, the provider; it never includes a credential. For `token_file` problems see the next row. |
| `local config load failed` mentions a `token_file` | The message names the problem and, except for the owner of the configuration file itself (`uses token_file but is owned by another user`: `chown` it to uid 65532 or `root`), the provider: `set only one of token and token_file`; `an authentication mode requires a token or a token_file`; `token_file must not be empty`; `token_file must be an absolute path`; `could not be opened` (the volume or Secret is not mounted, the path is wrong, or the file is not readable by the user running git-sync: for a Kubernetes Secret, check `fsGroup`); `is not a regular file`; `is writable by group or others` (use `0600`, `0640` or `0440`); `is owned by another user` (the owner must be uid 65532, the user running git-sync, or `root`; for a bind mount, `chown` the host file); `is empty`; `ends with a carriage return` (re-save with LF line endings); `has more than one line`; `contains control or invisible characters`; `has leading or trailing whitespace`; `is larger than the 4096-byte limit`. `connector config list`, `set`, `remove` and `status` fail the same way, so fix the file first, even to remove a provider. Run them as the user that runs git-sync: as `root` in a container (`docker exec -u 0`), files owned by uid 65532 count as owned by another user. |
| Log warns `credential file is readable by group or other users` | Expected for a Docker secret (`0444`) or a Kubernetes Secret. Where you control the mode, tighten it to `0600`; the connector starts either way. |
| Log warns `credential file is owned by another user` for the provider configuration | The connector starts for now, but a later release will refuse to. Make the user running git-sync (uid 65532 in the image) or `root` own the file. |
| Start fails with `temp directory … is not writable` | `/tmp` is not writable by uid 65532. In Docker, make sure `gitsync-tmp` is a writable volume; in Kubernetes, check that the `tmp` volume is mounted. |
| `cannot register: key directory … is not writable` | The `/data` volume is not writable by uid 65532. For a reused non-empty Docker volume, fix ownership once: `docker run --rm --user 0 --entrypoint chown -v gitsync-data:/data "$gitsync_image" -R 65532:65532 /data`. In Kubernetes, check the storage class supports `fsGroup`. |
| After a restart, the log shows `key file has unsafe permissions 0660; require owner-only access` and the connector will not connect | Something loosened the signing key's mode, typically a Kubernetes volume whose permissions are re-applied with the pod's `fsGroup` at every mount. The chart sets `fsGroupChangePolicy: OnRootMismatch` to avoid this; if it recurs after the fix below, your storage driver ignores that setting. Restore `0600` as described in [Restoring the key file mode](#restoring-the-key-file-mode). The key itself is unchanged, so no re-registration is needed. |
| A credential change did not take effect | git-sync reloads the provider configuration while it runs. Look in its log for `local provider configuration reloaded`, or for `the git-sync config cannot be used` with its reason (it then keeps the previous configuration). On Kubernetes the kubelet delivers an updated Secret to the pod after a delay, often a minute or more. See [Reloading the provider configuration](#reloading-the-provider-configuration). |
| `connector config set` fails on Kubernetes | A mounted provider-config Secret is read-only. Update the Secret (git-sync reloads it without a restart), or unset `gitSync.providerConfig.secretName` to manage the file with the CLI. |
| Log warns that syncing needs git 2.31 or newer | The image does not contain a usable git. Use an official image for your release ID. |

### Rejected configuration (`config_error`)

git-sync accepts a configuration from the platform only as a whole. If anything in it is not
acceptable, git-sync rejects all of it, keeps the previous configuration in effect (those
repositories keep syncing on their schedule, and nothing from the rejected one is used), logs
`git_sync_config rejected; leaving configuration unchanged` with the reason, and reports the reason
and the rejected version in its status as `config_error`. The next configuration that is accepted
clears it. While the latest configuration is rejected, [mirror cleanup](#mirror-cleanup) is
paused. The reason values are the connector's current wording and may be refined in later
releases.

Only the first two are about your installation. For the others the configuration came from the
platform, so there is nothing to change on your side: contact Memento with the reason.

| Reason | What it means | What to do |
|---|---|---|
| `replica_host_not_pinned` | `GIT_SYNC_REPLICA_HOST` (Helm: `gitSync.config.replicaHost`) is missing, empty, or not a valid `host` or `host:port`. git-sync stays connected but rejects **every** configuration and never pushes. The log says why at start; `connector status` shows what the process sees (it reads its own environment, so run it in the same container). | Set it to the replica host from your installation instructions (`host` or `host:port`, the port defaulting to 443; no scheme or path) and restart git-sync. |
| `replica_host_mismatch` | A repository's replica URL names a different host or port than you pinned, a scheme other than `https`, or something that cannot be shown to be the pinned host. The match is exact (no wildcards or subdomains) and applies to every repository, whether or not its origin is configured on your side. git-sync never pushes to any other host. | Compare the pin with the replica host and port in your installation instructions, including a non-default port as `host:port`; fix the pin and restart if it is wrong. If it is right, the platform sent a wrong URL: contact Memento. |
| `limits_out_of_range` | The configuration's schedule or limits has a value outside its accepted range. The log line `git_sync_config has a schedule or limit out of range` names the field (for example `interval_seconds`, accepted 60 to 86400). A value of the wrong type, such as a non-integer interval, is reported as `malformed_message` instead. | Contact Memento. |
| `too_many_repositories` | More than 4096 repositories in one configuration. | Contact Memento. |
| `invalid_repo_id` | A repository ID that is empty, longer than 256 characters, contains anything other than letters, digits, `.`, `_` and `-`, or is `.` or `..`. | Contact Memento. |
| `duplicate_repo_id` | Two entries with the same repository ID. | Contact Memento. |
| `invalid_config_version` | The configuration's version is a JSON object or array rather than a single value, or is longer than 256 bytes. | Contact Memento. |
| `malformed_message` | The configuration could not be read, or has no repository list. | Contact Memento. |

A repository whose replica URL or push token is merely unusable (a missing URL, one on the
pinned host but with a user name, query, or similar, or an empty push token) does not reject the whole configuration: if its origin host is
configured on your side, only that repository fails, as `unclassified` with the detail
`invalid replica configuration`.

### Restoring the key file mode

Run the one-off pod below with git-sync stopped (the volume can be mounted by one pod at a time).
Adjust the namespace and the claim name (`<release>-gitsync-data`), and use the same image as the
Deployment:

```bash
kubectl -n <namespace> scale deployment/mkonnect-gitsync --replicas=0
kubectl -n <namespace> run gitsync-fixperms --rm -i --restart=Never \
  --image=public.ecr.aws/h2a8k0r3/memento-gitsync:<release-id> \
  --overrides='{"spec":{"securityContext":{"runAsUser":65532,"runAsGroup":65532},"containers":[{"name":"gitsync-fixperms","image":"public.ecr.aws/h2a8k0r3/memento-gitsync:<release-id>","command":["sh","-c","ls -ln /data/gitsync-key && chmod 600 /data/gitsync-key && ls -ln /data/gitsync-key"],"volumeMounts":[{"name":"data","mountPath":"/data"}]}],"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"mkonnect-gitsync-data"}}]}}'
kubectl -n <namespace> scale deployment/mkonnect-gitsync --replicas=1
```

The first `ls` shows the current mode and the second confirms `-rw-------`. This pod sets no
`fsGroup`, so nothing re-applies group permissions while it runs.

For connection problems with the gateway itself (repeating `gateway connection failed` warnings,
registration errors), the causes and fixes are the same as for the API connector; see the
[Installation guide's troubleshooting](installation.md#troubleshooting).
