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

git-sync syncs when the platform sends or refreshes the repository configuration and when it asks
for an immediate sync. The first sync of a repository downloads all of its history; later syncs
transfer only what changed.

Your git server's credential stays on your side: it is stored in git-sync's local configuration
and used only for requests to your git server. It is never sent to Memento.

## Before you start

### Requirements

- **A runtime:** Docker Engine, or a Kubernetes cluster with Helm 3.8+ (OCI support), as for the
  [connector](installation.md#system-requirements).
- **A git-sync connector in the Memento portal.** Create it on the portal's git-sync install page
  and note its values (see below). It has its own connector ID, which is **not** the API
  connector's.
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

### What you'll collect from the Memento portal

| Value | Used as | Notes |
|---|---|---|
| Gateway URL | `GATEWAY_URL` | Copy it **exactly** from the portal; it must be `wss://`. |
| Connector ID | `CONNECTOR_ID` | The ID of the **git-sync** connector, not the API connector's. |
| Replica host | `GIT_SYNC_REPLICA_HOST` | A host name, optionally with `:port`; no `https://`, no path. git-sync pushes only to this host. |
| Registration token | read from a file | **One-time**, used only on first start. |
| Release ID / chart version | image tag / `--version` | Pin an explicit version; never a moving tag. |

The registration token is a one-time secret: it is consumed on first startup, after which the
connector authenticates with the private key it was issued. git-sync reads the token **from a
file** (`REGISTRATION_TOKEN_FILE`), not from an environment variable, so it does not show up in
`docker inspect` or in the pod specification.

### Sizing

- **Data volume** (`/data`): the sum of the mirrored repositories' sizes, multiplied by about 1.2,
  plus some headroom for growth. A mirror holds the full history of every branch and tag, so it is
  about the size of a bare clone. The Helm chart default is `20Gi`.
- **Scratch space** (`/tmp`): git unpacks and repacks objects while it fetches and pushes. Up to
  four repositories sync at once, so plan for the working set of your largest few repositories. It
  must be backed by disk, not memory. The Helm chart default is a `5Gi` disk-backed `emptyDir`.
- **Memory:** a very large repository, or a large first sync, can exceed the default memory limit;
  the repository is then reported as `job_out_of_memory` (see [Troubleshooting](#troubleshooting)).

## Option A: Docker

Run git-sync as a second container, with its **own** volumes. Do not reuse the API connector's
volume. The container is started read-only with no Linux capabilities, and with a separate
disk-backed volume for scratch space at `/tmp`.

The one-time registration token is kept in a small Docker volume, `gitsync-secrets`, instead of on
the host file system or in an environment variable. git-sync reads it from a file and refuses a
token file that others can access, so the file must be owned by the container user (uid 65532)
with owner-only permissions. The steps below create it that way without needing root on the host:
a one-off container writes the token from standard input, so it never appears on a command line
or in your shell history. Run this snippet from Bash or Zsh:

```bash
umask 077
gitsync_dir="${XDG_CONFIG_HOME:-$HOME/.config}/mkonnect-gitsync"
mkdir -p "$gitsync_dir"
chmod 700 "$gitsync_dir"

cat > "$gitsync_dir/gitsync.env" <<'EOF'
GATEWAY_URL=<gateway-url-from-portal>
CONNECTOR_ID=<git-sync-connector-id-from-portal>
GIT_SYNC_REPLICA_HOST=<replica-host-from-portal>
KEY_FILE=/data/gitsync-key
EOF

# Used only for the first start; removed once the portal shows the connector as connected.
printf 'REGISTRATION_TOKEN_FILE=/run/secrets/registration-token\n' > "$gitsync_dir/registration.env"

# Store the token in a volume, as a file owned by uid 65532 with mode 0600. The first container
# makes the (empty) volume writable for that user; the second writes the token as that user.
docker volume create gitsync-secrets
docker run --rm -v gitsync-secrets:/secrets alpine \
  sh -c 'chown 65532:65532 /secrets && chmod 700 /secrets'
printf 'Paste registration token: ' >&2
IFS= read -r -s GITSYNC_TOKEN
printf '\n' >&2
printf %s "$GITSYNC_TOKEN" | docker run --rm -i --user 65532:65532 -v gitsync-secrets:/secrets alpine \
  sh -c 'umask 077; cat > /secrets/registration-token'
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
  public.ecr.aws/h2a8k0r3/memento-gitsync:<release-id>
```

Notes:

- `gitsync-data` is where the private key (`/data/gitsync-key`), the provider configuration
  (`/data/git-sync.json`), and the mirrors (`/data/mirrors`) live. Removing it deletes all three.
- `gitsync-tmp` is scratch space only and can be recreated at any time. Docker does not cap the
  size of a volume, so make sure the host has room for it.
- Keep `gitsync-data`, `gitsync-tmp`, and `gitsync-secrets` separate from the API connector's
  `mkonnect-data` volume.
- `gitsync-secrets` is mounted read-only (`:ro`). The one-off `alpine` containers only prepare it;
  any small image with a shell works.
- If your git server is on the Docker host or a sibling container, the same networking rules as for
  the API connector apply; see [Reaching a tool from Docker](setup.md#reaching-a-tool-from-docker).
- To go through an HTTP proxy, add `HTTPS_PROXY`, `HTTP_PROXY`, and `NO_PROXY` to `gitsync.env`;
  git honours them.

Next, [configure your git server credential](#configure-your-git-server-credential), then
[verify the connection](#verify) and [remove the registration token](#remove-the-registration-token).

## Option B: Kubernetes (Helm)

git-sync is part of the `mkonnect` chart and is **off by default**. Enabling it adds a separate
Deployment, its own `PersistentVolumeClaim`, and a `NetworkPolicy`; it does not change the API
connector's resources. The commands below add git-sync to an existing release created as in the
[Installation guide](installation.md#option-b--kubernetes-helm). For a new release, also pass the
API connector's `--set` options from that guide.

Create the registration token as a Secret from an owner-only file. Run this snippet from Bash or
Zsh:

```bash
umask 077
gitsync_dir="${XDG_CONFIG_HOME:-$HOME/.config}/mkonnect-gitsync"
mkdir -p "$gitsync_dir"
chmod 700 "$gitsync_dir"
: > "$gitsync_dir/registration.token"
chmod 600 "$gitsync_dir/registration.token"
printf 'Paste registration token: ' >&2
IFS= read -r -s GITSYNC_TOKEN
printf '\n' >&2
printf %s "$GITSYNC_TOKEN" > "$gitsync_dir/registration.token"
unset GITSYNC_TOKEN

kubectl create secret generic mkonnect-gitsync-registration \
  --from-file=registration-token="$gitsync_dir/registration.token"

helm upgrade --install mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --version <chart-version> --reuse-values \
  --set gitSync.enabled=true \
  --set gitSync.image.repository=public.ecr.aws/h2a8k0r3/memento-gitsync \
  --set gitSync.config.connectorId=<git-sync-connector-id-from-portal> \
  --set gitSync.config.replicaHost=<replica-host-from-portal> \
  --set gitSync.config.registrationTokenSecret.name=mkonnect-gitsync-registration
```

The chart mounts the token read-only at `/etc/mkonnect-gitsync/registration-token/` (mode `0440`)
and points `REGISTRATION_TOKEN_FILE` at it. Instead of a Secret you manage, you can let the chart
create one with `--set-file gitSync.config.registrationToken="$gitsync_dir/registration.token"`.
Either way the token is delivered as a file; it is never an environment variable.

What the chart sets up:

| Value | Default | Meaning |
|---|---|---|
| `gitSync.enabled` | `false` | Turns the add-on on. When `false` nothing git-sync-related is rendered. |
| `gitSync.image.repository` | (required) | The git-sync image, e.g. `public.ecr.aws/h2a8k0r3/memento-gitsync`. |
| `gitSync.image.tag` | chart `appVersion` | The release ID. Selecting the chart version selects the matching image. |
| `gitSync.image.digest` | empty | Optional `sha256:` digest. When set, the image is `repository@digest` and the tag is ignored. |
| `gitSync.config.connectorId` | (required) | The git-sync connector's own ID. Must differ from `config.connectorId`. |
| `gitSync.config.replicaHost` | (required) | The replica host from the portal; host or `host:port`. The chart rejects a scheme or a path. |
| `gitSync.config.gatewayUrl` | `config.gatewayUrl` | Override only if the portal gives git-sync a different gateway URL. |
| `gitSync.config.registrationTokenSecret.name` / `.key` | empty / `registration-token` | An existing Secret holding the one-time token (preferred). |
| `gitSync.config.registrationToken` | empty | Alternative: the chart creates the Secret from this value (use `--set-file`). |
| `gitSync.providerConfig.secretName` / `.key` | empty / `git-sync.json` | An existing Secret holding the git server credentials; see below. |
| `gitSync.caBundle.secretName` / `.key` / `.mountPath` | empty / `ca-bundle.pem` / `/etc/mkonnect-gitsync-ca` | A PEM bundle for git to trust; see below. |
| `gitSync.persistence.size` / `.storageClass` | `20Gi` / cluster default | The git-sync volume. |
| `gitSync.tmp.sizeLimit` | `5Gi` | Size limit of the disk-backed `/tmp` scratch volume. |
| `gitSync.resources` | 250m / 512Mi requests, 1 / 2Gi limits | CPU and memory. |
| `gitSync.networkPolicy.enabled` | `true` | Deny all ingress to git-sync pods; see the [security guide](git-sync-security.md#network-policy). |
| `gitSync.networkPolicy.egress.cidrs` | empty | Optional egress allow-list; empty leaves egress unrestricted. |

See [`charts/mkonnect/values.yaml`](../charts/mkonnect/values.yaml) for every option.

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
and set `GIT_SSL_CAINFO` to its path in `gitsync.env`.

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
docker restart mkonnect-gitsync
```

**Kubernetes.** The same commands work with `kubectl exec -i deploy/mkonnect-gitsync --
/mkonnect-gitsync connector ...` when you let git-sync manage the file in its volume
(`/data/git-sync.json`, owner-only). The recommended approach is to keep the credential in a Secret
you control (which can itself come from Vault or the External Secrets Operator) and mount it. The
creation step uses a short-lived local file, so create it with restrictive permissions outside your
checkout and remove it afterwards:

```bash
umask 077
secret_dir="$(mktemp -d)"
providers_file="$secret_dir/git-sync.json"
cat > "$providers_file" <<'EOF'
{
  "internal-git": {
    "base_url": "https://git.example.com",
    "auth": "bearer",
    "token": "REPLACE_WITH_ACCESS_TOKEN"
  }
}
EOF
# Replace the placeholder in a local editor; do not paste a real token into this shell block.
${EDITOR:-vi} "$providers_file"
kubectl create secret generic mkonnect-gitsync-providers \
  --from-file=git-sync.json="$providers_file" && rm -r "$secret_dir"

helm upgrade mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --version <chart-version> --reuse-values \
  --set gitSync.providerConfig.secretName=mkonnect-gitsync-providers
```

Each entry has `base_url` (required), `auth` (`bearer`, `basic`, or omitted for none), `username`
(basic only), and `token`. Unknown fields are rejected. The Secret is mounted read-only at
`/etc/mkonnect-gitsync-providers/` (mode `0440`), outside the data volume, so `connector config
set` cannot modify it: change it by updating the Secret. The mirrors stay on the data volume under
`/data/mirrors`.

**Changes need a restart.** git-sync reads its provider configuration once, at startup; there is
no reload signal. After any change, including rotating or revoking a credential:

```bash
docker restart mkonnect-gitsync                           # Docker
kubectl rollout restart deployment/mkonnect-gitsync       # Kubernetes
```

A restart also re-runs the check that the configured git servers are reachable. `connector status`
lists the configured providers; the state of enrollment and of each repository is shown in the
Memento portal.

## Verify

git-sync should register and connect within a few seconds.

- **Portal:** the git-sync connector shows as connected, with a recent heartbeat. Once a provider
  is configured, the portal also shows whether your git server is reachable and accepts the
  credential, before any repository is added. After you add repositories, each shows its sync
  state.
- **Docker logs:**

  ```bash
  docker logs mkonnect-gitsync
  ```

  A healthy start logs `mkonnect-gitsync starting` with the connector ID, the gateway, and the
  number of configured providers. When the platform sends repositories you will see
  `git_sync_config accepted`, then `sync complete` (or `sync failed` with an error type) per
  repository.
- **Kubernetes:** `kubectl logs deploy/mkonnect-gitsync` and
  `kubectl get pods -l app.kubernetes.io/name=mkonnect-gitsync`.

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
rm "$gitsync_dir/registration.env"
docker rm -f mkonnect-gitsync
docker run -d --name mkonnect-gitsync --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  -v gitsync-data:/data \
  -v gitsync-tmp:/tmp \
  --env-file "$gitsync_dir/gitsync.env" \
  public.ecr.aws/h2a8k0r3/memento-gitsync:<release-id>
docker volume rm gitsync-secrets
```

**Kubernetes:** stop mounting it and delete the Secret:

```bash
gitsync_dir="${XDG_CONFIG_HOME:-$HOME/.config}/mkonnect-gitsync"
rm "$gitsync_dir/registration.token"
helm upgrade mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --version <chart-version> --reuse-values \
  --set gitSync.config.registrationTokenSecret.name="" \
  --set gitSync.config.registrationToken=""
kubectl delete secret mkonnect-gitsync-registration
```

If you passed the token with `--set-file`, earlier Helm release revisions still contain it; see the
note in the [Installation guide](installation.md#remove-the-registration-token-after-verification).
Using an existing Secret, as above, keeps the token out of Helm release history entirely.

## Upgrading

Releases use calendar identifiers (`YYYYMMDD.n`). Always pin an explicit release.

**Docker:** pull the new release ID and recreate the container with the same volumes and env file:

```bash
gitsync_dir="${XDG_CONFIG_HOME:-$HOME/.config}/mkonnect-gitsync"
docker pull public.ecr.aws/h2a8k0r3/memento-gitsync:<new-release-id>
docker rm -f mkonnect-gitsync
docker run -d --name mkonnect-gitsync --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  -v gitsync-data:/data \
  -v gitsync-tmp:/tmp \
  --env-file "$gitsync_dir/gitsync.env" \
  public.ecr.aws/h2a8k0r3/memento-gitsync:<new-release-id>
```

**Helm:** the default image tag is the chart's `appVersion`, so upgrading the chart upgrades both
the API connector and git-sync:

```bash
helm upgrade mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --version <new-chart-version> --reuse-values
```

If you pinned `gitSync.image.tag` or `gitSync.image.digest`, update that value instead.

The key, provider configuration, and mirrors are on the volume, so the connector reconnects with
its existing key and syncs incrementally. A sync that is running when the container stops is
cancelled and starts again afterwards. If the platform reports that a critical security update is
required, git-sync stops syncing (repositories stay `pending`) until you upgrade.

## Uninstalling

**Docker:**

```bash
docker rm -f mkonnect-gitsync
docker volume rm gitsync-data gitsync-tmp    # deletes the private key, provider config, and all mirrors
```

**Helm:**

```bash
helm upgrade mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --version <chart-version> --reuse-values --set gitSync.enabled=false
```

This removes the git-sync Deployment, ServiceAccount, NetworkPolicy, and `PersistentVolumeClaim`
(`mkonnect-gitsync-data`), and leaves the API connector untouched. Your storage class's reclaim
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

| Symptom or error type | Likely cause and fix |
|---|---|
| `origin_access_denied` | Your git server rejected the credential (401, 403), or the repository was not found (a 404; many servers answer 404 for a repository the credential cannot see). Check the token's permissions and expiry. It is also reported, with the detail `host not configured locally`, when no provider matches the repository's scheme, host, and port: run `connector config list` and add the provider for that exact host. |
| `origin_unreachable` | git-sync could not connect to your git server: DNS, firewall, proxy, a TLS certificate it does not trust (see [Trusting an internal CA](#trusting-an-internal-ca-for-git)), a server error (5xx), or a fetch that ran past its 30-minute limit. Test from the host that runs git-sync. |
| `replica_unreachable` | git-sync could not connect to the Memento replica. Allow outbound HTTPS to the replica host from the portal. If you set a CA bundle for git, it must also contain the public root(s) for the replica. |
| `credential_rejected` | The **replica** rejected the push token (not your git credential). Tokens are issued by the platform and refreshed while the connector is connected. If it persists, check that the connector is connected and its host clock is correct, then contact Memento. |
| `push_token_expired` | The push token was past its expiry when a push was due. Usually the host clock is wrong, or the connector was disconnected and could not receive a refreshed token. Check NTP and the connection. |
| `replica_rejected` | The replica refused the push: a rejected ref, a policy decision, or a size or quota limit. Contact Memento with the repository ID. |
| `push_timeout` | A push ran past its 30-minute limit, typically a very large first push over a slow link. Check bandwidth to the replica; later syncs push less. |
| `local_disk_exhausted` | The data volume or the `/tmp` scratch space is full. Grow `gitSync.persistence.size` (or the Docker volume's backing disk) or `gitSync.tmp.sizeLimit`; see [Sizing](#sizing). |
| `job_out_of_memory` | git was killed for lack of memory. Raise the memory limit (`gitSync.resources.limits.memory`, or `--memory` with Docker). |
| `unclassified` | A failure that does not fit the types above. The report carries a short, scrubbed detail line; `docker logs` / `kubectl logs` have the same. The detail `origin has no branches or tags; replica left unchanged` means the origin is empty or advertised no branches or tags; git-sync deliberately does not touch the replica in that case. |
| The portal says the configuration was rejected (`replica_host_mismatch` or `replica_host_not_pinned`) | `replica_host_not_pinned`: `GIT_SYNC_REPLICA_HOST` is not set (the chart requires `gitSync.config.replicaHost`). `replica_host_mismatch`: the replica the platform sent is on a different host than the one you configured. Re-copy the replica host from the portal; if it is correct, contact Memento. git-sync never pushes to a host other than the one configured. |
| Repositories stay `pending`, and the log says a critical security update is required | The platform requires an upgrade before syncing continues. Upgrade to the latest release. |
| A repository stays `syncing` for a long time | The first sync of a large repository is slow, or other repositories are ahead of it (four sync at a time). Watch `kubectl logs` / `docker logs` for the outcome. |
| Container exits at start with `configuration error` | `GATEWAY_URL` must be an absolute `wss://` URL, and `CONNECTOR_ID` must be set. Check the logs. |
| Start fails with `local config load failed` | The provider configuration is invalid: not valid JSON, an unknown field, a token shorter than 16 characters, an authentication mode on a non-`https` URL, two providers for one host, or a file readable by others. The message names the cause; it never includes a credential. |
| `cannot register: key directory … is not writable` | The `/data` volume is not writable by uid 65532. For a reused non-empty Docker volume, fix ownership once: `docker run --rm -v gitsync-data:/data alpine chown -R 65532:65532 /data`. In Kubernetes, check the storage class supports `fsGroup`. |
| A credential change did not take effect | The provider configuration is read at startup. Restart git-sync; see [Configure your git server credential](#configure-your-git-server-credential). |
| `connector config set` fails on Kubernetes | A mounted provider-config Secret is read-only. Update the Secret and restart, or unset `gitSync.providerConfig.secretName` to manage the file with the CLI. |
| Log warns that syncing needs git 2.31 or newer | The image does not contain a usable git. Use an official image for your release ID. |

For connection problems with the gateway itself (repeating `gateway connection failed` warnings,
registration errors), the causes and fixes are the same as for the API connector; see the
[Installation guide's troubleshooting](installation.md#troubleshooting).
