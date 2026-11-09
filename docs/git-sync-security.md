# git-sync: security and data handling

This page is for security and network reviewers evaluating git-sync before approving it. It
describes what git-sync connects to, what data it moves and stores, who holds which credential,
and how the process is isolated. For installation steps, see the [git-sync guide](git-sync.md).

It describes what the open-source code in this repository does. You can verify any statement here
against [`internal/gitsync`](../internal/gitsync) and [`cmd/mkonnect-gitsync`](../cmd/mkonnect-gitsync).

## Summary

- git-sync is **outbound-only**. It listens on no port, has no Service, and the chart, by default
  (`gitSync.networkPolicy.enabled`, with a CNI that enforces `NetworkPolicy`), denies all ingress to
  its pods.
- It **reads** from your git server and **writes** to exactly one Memento replica host, which you
  pin at install time.
- It replicates **branches and tags only** (`refs/heads/*` and `refs/tags/*`).
- Your git server credential stays on your side, is scoped to your git server's address, and is
  never sent to Memento.
- The push credential for the replica is issued by the platform, held **in memory only**, and is
  never written to disk by git-sync.
- The process runs as an unprivileged user with a read-only root filesystem and no Linux
  capabilities.
- Release images are built by this repository's release workflow and are meant to be signed, with
  build provenance. Releases up to and including 20261022.0 are not signed; see
  [Supply chain](#supply-chain).

## Network destinations

| Destination | Port | Direction | Purpose |
|---|---|---|---|
| Memento Bridge Gateway (the Gateway URL in the installation instructions you were given for your tenant) | 443 (`wss://`), unless that URL names another port | outbound | Registration on first start, authentication, heartbeat and status, and receiving repository configuration and requests for an immediate sync. |
| Memento replica host (the replica host in the same instructions) | 443, or the port you pinned | outbound | HTTPS `git push` of branches and tags. The replica accepts TLS 1.3 only, and only connections from public addresses Memento has allowed for you (see below). |
| Your git server(s): the base URL(s) you configure as providers | as in each base URL (443 for `https://`) | outbound | Read-only `git ls-remote` and `git fetch`. |
| Container registry (`public.ecr.aws`) | 443 | outbound, from the container runtime or the cluster nodes, and from where you run `helm` | Pulling the image at install and upgrade, and, on Kubernetes, pulling the Helm chart (`oci://public.ecr.aws/...`). The running git-sync process does not contact it. |
| DNS | 53 | outbound | Name resolution. |

Verifying a release before you install it is done by you, from your own machine, not by git-sync.
It needs `github.com` (the release metadata, and build provenance attestations) and, for the
signature, Sigstore's public infrastructure, as described in the
[cosign documentation](https://docs.sigstore.dev/cosign/system_config/installation/); none of that
is contacted by the running connector.

The replica is reached over HTTPS with TLS 1.3 only, and it accepts connections only from public
addresses that Memento has allowed for you; Memento allows them before the first push, so the
addresses are sent to Memento first (see [Requirements](git-sync.md#requirements) in the
git-sync guide). git-sync itself neither sets a TLS version nor knows the allowed addresses: both
are enforced by the replica, and a mismatch shows up as a failed connection (see
[Troubleshooting](git-sync.md#troubleshooting)).

There are no inbound connections. Exact gateway and replica host names are given in the
installation instructions for your tenant; they are not listed here because they are specific to
it. The
connector refuses to push to any host other than the pinned replica host and only ever uses a
credential against git servers that you configured (see
[How the credentials are kept to their destination](#how-the-credentials-are-kept-to-their-destination)).

If your environment requires an HTTP proxy, set `HTTPS_PROXY`, `HTTP_PROXY`, and `NO_PROXY`, and
their lower-case forms, in the container environment (the chart's `gitSync.proxy.*` values set
both cases). Of its own environment, git-sync passes on to git only `PATH`, these six variables, and
the three CA settings (`SSL_CERT_FILE`, `SSL_CERT_DIR`, `GIT_SSL_CAINFO`; see
[Process isolation and hardening](#process-isolation-and-hardening)), for its fetches and pushes; git's
HTTP library reads only the lower-case `http_proxy` for `http://` URLs. The connection to the
gateway uses Go's default HTTP transport, which reads `HTTPS_PROXY` / `https_proxy` and `NO_PROXY` /
`no_proxy` for its `wss://` connection. A proxy that tunnels with `CONNECT` sees the destination
host names but not the TLS-protected content. The gateway connection through a proxy depends on
the proxy allowing WebSocket upgrades; it has not been tested through a proxy here.

## Data flow

```
 your git server ──fetch (read-only)──▶ local bare mirror (on the git-sync volume) ──push──▶ Memento replica
        ▲                                         │
        └── credential used here only             └── push token used here only

 git-sync ◀── wss:// ──▶ Memento Bridge Gateway   (control messages only; no repository data)
```

For each configured repository, one sync does the following:

1. `git fetch --prune --force` of `refs/heads/*` and `refs/tags/*` from your git server into a
   bare repository on the data volume. The mirror is created on first use.
2. A check that the mirror holds at least one branch or tag. If your git server advertises none,
   the sync stops with an error (type `unclassified`, detail `origin has no branches or tags;
   replica left unchanged`) and **does not touch the replica**. This prevents an origin that
   advertises no refs at all from deleting everything that was replicated.
3. `git push --prune --force` of the same two ref namespaces to the replica. A branch or tag that
   no longer exists on your git server is removed from the replica. The push is forced, so a
   force-push or history rewrite on your git server moves the same branch or tag on the replica
   (see [What is copied, and what is not](git-sync.md#what-is-copied-and-what-is-not)). When the
   platform sets a push
   batch size and the mirror holds more refs than that, git-sync first asks the replica what a
   push would change (a request that sends no data); if more refs than the batch size would be
   written, they go in batches, and the removals are made by one final push only once every batch
   has succeeded (see [Limits the platform can send](git-sync.md#limits-the-platform-can-send)).

Repository data travels directly between your git server, git-sync, and the replica. The push to
the replica is always HTTPS. The fetch from your git server uses the scheme of the base URL you
configure; a provider with a credential must be `https://`, and only a provider without one may be
`http://`. The gateway tunnel carries only control messages: configuration from the platform
(including the replica push URL and push token for each repository), a request for an immediate
sync (`git_sync_now`), and status (repository IDs, states, error types) from the connector. The
platform can request an immediate sync at any time; git-sync does not rate-limit those requests
beyond folding them into one rerun per repository.

### What the replica receives

Only `refs/heads/*` and `refs/tags/*`, together with the objects reachable from them. That is the
complete history behind every branch and tag, including files that were later deleted. It does
**not** receive other namespaces your server may keep (for example pull or merge request refs),
Git LFS object contents (git-sync uses plain `git fetch` and `git push`; a repository that appears
to use LFS is reported with the warning `lfs_not_replicated`), the repositories that submodules
point to, repository hooks or settings, anything your git server keeps outside the repository's
branches and tags (such as issues, merge-request discussions and wikis), or anything outside the
repository. Review what the repositories you select contain
before replicating them.

### What is stored, and where

| Data | Where | Notes |
|---|---|---|
| Signing key (ML-DSA-65 private key) | `/data/gitsync-key` on the data volume | Mode `0600`, owner-only. Issued by the gateway at registration. Losing it means re-registering. |
| Provider configuration, including your git server credential | A Secret mounted read-only at `/etc/mkonnect-gitsync-providers/`, or `/data/git-sync.json` on the data volume | In **cleartext** (not encrypted at rest by git-sync). git-sync tightens the file to owner-only (`0600`) where it can, and refuses to start with a read-only file that is group-writable or accessible by others. |
| Mirrors of the replicated repositories | `/data/mirrors` on the data volume | A full copy of the history behind every branch and tag. Treat the volume as containing your source code. The mirror of a repository the platform stops listing gets a small timestamp marker file and is deleted once it has been unlisted for seven days (see [Mirror cleanup](git-sync.md#mirror-cleanup)); `GIT_SYNC_MIRROR_GC=off` turns that off. |
| Run state | `/data/mirrors/run-state.json` on the data volume | Per repository: when the last attempt started and how long it took, when the last sync succeeded, how many refs it pushed, the newest commit time and default branch at your git server, and an unsalted SHA-256 hash of the origin and replica URLs (it does not reveal them, but could confirm a guessed pair). No credential, URL, host name, path, or error text. Mode `0600`. |
| Replica push tokens | **Memory only** | Received from the platform per repository, with an expiry, and kept only for repositories that can be synced. Not written to disk and not logged. |
| One-time registration token | A read-only file mounted from a Secret (Kubernetes) or from a dedicated Docker volume | Used once. Remove it after the connector has registered. The chart and the Docker instructions deliver it as a file (`REGISTRATION_TOKEN_FILE`), not as an environment variable. The connector also accepts `REGISTRATION_TOKEN` in its environment, which you should not use: an environment value shows in `docker inspect` and in the pod specification. |
| Per-sync scratch data | `/tmp` (disk-backed) | Each sync creates a throwaway, empty home directory for git, also used for git's temporary files, and removes it afterwards. The fetched objects themselves go into the mirror, not here. |
| Logs | Standard error (`docker logs`, `kubectl logs`) | See [What is logged](#what-is-logged). |

git-sync does not modify your git server. Any change there (revoking a token, deleting a
repository) takes effect at the next sync.

### What is reported to the platform

The connector sends status: for each repository its ID, state (`pending`, `syncing`, `ok`,
`error`), an error type, any warning codes (currently only `lfs_not_replicated`), and, for failures
it cannot classify, a single short detail line from which URLs, host names, and absolute paths have been removed. It also sends, for each repository,
the times of its last attempt and last success, how long the attempt took, how many refs it
pushed, the newest commit time across the branches and tags, and the default branch name at your
git server. It also sends the configuration version it accepted, the reason when it rejected a
configuration, whether syncing is suspended pending a critical update, the earliest push-token
expiry it holds, a per-process boot ID and sequence number, and one aggregated reachability result
for your configured git servers (`reachable`, `access_denied`, or `unreachable`; the worst outcome
across them, not one per server) with the time it was checked. That result is re-checked about
every 30 seconds until a configuration is accepted and about every 5 minutes afterwards, and is
absent when no provider is configured, before the first check has finished, and when the check
cannot run. It never sends your git server credential, a repository's
contents, or file paths.

### What is logged

git-sync logs to standard error. The log contains:

- A startup line with the connector ID, the **gateway URL**, the protocol version, the connector
  version, and the number of configured providers; and the pinned replica host.
- Connection events (for example repeated `gateway connection failed` warnings) and the receipt of
  each configuration (`git_sync_config accepted`, or a rejection with its reason).
- One line per finished sync: the repository ID, success or the error type, a short detail, and
  the duration. For failures git-sync cannot classify, the detail has URLs, host names, and
  absolute paths removed before it is logged.
- Error messages for local faults, which **can include local file paths** (for example the path of
  the key file or the provider configuration file) and the name of a setting that is wrong.

Your git server credential and the replica push token are held in self-redacting types and are
not written to the log. If a server repeats a credential back in an error message, the text it sent
is not reported, and the credential is also removed from git's output before it is read. Repository IDs are restricted to a safe character set. Treat the log as
operational data: it identifies your connector and gateway, though not your git servers' host
names or repository paths.

## Credential custody

| Credential | Held by | How it is protected | Lifetime |
|---|---|---|---|
| Registration token | Issued for your tenant at registration; delivered to git-sync as a file | Mounted read-only, mode `0440` in Kubernetes (group-readable through the pod's `fsGroup`); in Docker, a `0600` file owned by uid 65532 in a dedicated volume mounted read-only. git-sync refuses a token file that is accessible by others or writable by its group. Delivered as a file in both, so it is not shown by `docker inspect` or in the pod specification (see the note on `REGISTRATION_TOKEN` above). | One time; remove after registration. |
| Signing key | Generated by the gateway at registration; stored by git-sync | Owner-only file on the data volume (git-sync refuses a key file with any group or other permission); the signing key is never sent again. Each reconnect proves possession by signing a challenge. | Until you delete the volume. |
| Git server credential | You | Stored in the provider configuration, or in a file the configuration names (`token_file`; refused when group or others can write it, or when its owner is neither the user running git-sync nor root). Used only for requests to the matching git server. On Kubernetes, prefer a Secret you manage. | Until you rotate or revoke it at your git server. |
| Replica push token | Issued by the platform per repository | Memory only; sent only to the pinned replica host; carries an expiry that git-sync checks before pushing. | Has an expiry; refreshed by the platform while connected. |

Both git credentials reach `git` as configuration in the child process's environment, scoped to
one URL, never on the command line, so they do not appear in a process listing. Like any
environment variable, they are readable by the same user (`/proc/<pid>/environ`), which is the
unprivileged user the process itself runs as.

### How the credentials are kept to their destination

- **Host-match guard.** The platform tells git-sync which origin URL to mirror for each
  repository. git-sync uses a credential only if that URL has the same scheme, host, and port as a
  provider *you* configured; otherwise the repository is rejected and nothing is contacted. The
  URL is validated and re-serialized (no embedded credentials, no unusual host spellings such as
  numeric or internationalized forms), and git is given the validated form, so the host that was
  checked is the host that is dialled.
- **Scoped header.** The credential is sent as an HTTP header that git attaches only to requests
  under the matching URL. The origin credential is never presented to the replica, nor the push
  token to your git server. Redirects are not followed, so a redirect cannot carry a header to
  another host.
- **Replica host pin.** The replica push URL also comes from the platform. git-sync accepts it only
  if it is an `https` URL on exactly the host and port you set in `GIT_SYNC_REPLICA_HOST` (the
  chart's `gitSync.config.replicaHost`; the port defaults to 443) at install time. The match is
  exact: there are no wildcards, subdomains, or suffix matches, and a host that cannot be shown to
  be the pinned one is treated as a different host. The pin is read once at startup and cannot be
  changed by the platform. A configuration in which any repository names another replica host is
  rejected as a whole (reason `replica_host_mismatch`) and the previous configuration stays in
  effect. If the pin is missing, empty, or malformed, git-sync rejects every configuration (reason
  `replica_host_not_pinned`) and never pushes, while still connecting and reporting status. The pin
  is checked again just before a sync starts, which is the point where the push token would leave
  the process.

### How the credentials reach git

For each fetch, probe or push, git-sync builds a new environment for the `git` child process and adds
the credential as git configuration through `GIT_CONFIG_COUNT` and `GIT_CONFIG_KEY_n` /
`GIT_CONFIG_VALUE_n` (this is why git 2.31 or newer is required). The credential is an
`Authorization` header set under `http.<url>/.extraHeader`, so git sends it only to requests whose
URL falls under that one URL (same scheme, host and port). It is not placed on the command line, in
the URL, in a credential helper, in git's system or global configuration (both are disabled), or in
a file; the process's own environment is not inherited by git. The environment of a child is readable
by the same user, as noted above.

### Rotation, revocation and live reload

- **Your git server credential** is revoked or rotated by changing the provider configuration (or a
  `token_file` it names), or by revoking it at your git server. git-sync reads the provider
  configuration every 5 seconds (and at once on `SIGHUP`), applies a change after seeing it twice
  one second apart, stops any sync that was using a removed or changed credential (its git processes
  are killed and the sync pushes nothing), and syncs the repository again with the new one. Deleting
  the provider file revokes every provider credential. A file that cannot be used is not applied: git-sync keeps
  the last configuration that worked and logs the kind of problem. On Kubernetes, a Secret reaches
  the pod after a kubelet delay. See
  [Reloading the provider configuration](git-sync.md#reloading-the-provider-configuration).
- **A credential your git server refuses** is not retried in a loop: the repositories that use it
  wait, which avoids piling up failed logins. See
  [A rejected credential holds back every repository that uses it](git-sync.md#a-rejected-credential-holds-back-every-repository-that-uses-it).
- **The replica push token** is issued and refreshed by the platform, not by you. git-sync does not
  push with a token it knows has expired. You cannot rotate it; stopping git-sync stops its use.
- **What needs a restart:** the replica host pin, the registration token file, the signing key file
  and every other setting are read once at startup.
- **Stopping git-sync** ends all use of every credential it holds. On a normal shutdown it stops
  its running git processes and waits up to 10 seconds for them, so none holding a credential
  outlives it.

## Process isolation and hardening

The Helm chart and the Docker instructions apply the following:

- Runs as a non-root user (uid and gid `65532`). With Docker the image ships `/data` owned by that
  user. On Kubernetes the data volume's root is group-owned by `65532` through the pod's `fsGroup`
  (the volume itself is provisioned by your storage class).
- **Read-only root filesystem.** The writable paths this configuration provides are the data
  volume (`/data`) and the scratch volume (`/tmp`); the scratch volume is disk-backed, not memory.
  The container runtime also provides a small in-memory `/dev/shm`, which these settings do not
  manage or restrict.
- **No Linux capabilities** (`--cap-drop ALL`; `capabilities.drop: [ALL]`), and **no privilege
  escalation** (`--security-opt no-new-privileges`; `allowPrivilegeEscalation: false`).
- **Runtime-default seccomp profile** in Kubernetes.
- **No ServiceAccount token** is mounted into the pod (`automountServiceAccountToken: false`), and
  the chart creates a dedicated ServiceAccount with no permissions.
- **No listening port and no Service.** The health probe runs `/mkonnect-gitsync --health` inside
  the container.
- **Secrets as read-only mounts** with restrictive modes (`0440`): the registration token and the
  optional provider configuration. The provider configuration is mounted outside the data volume,
  and mirrors are always written under `/data/mirrors`, never into the read-only mount.
- **`fsGroupChangePolicy: OnRootMismatch`**, so the kubelet does not re-apply group permissions to
  the signing key on every mount.
- **One replica, `Recreate` rollout**, with a volume of its own. git-sync never shares the API
  connector's volume or key, and uses its own connector ID.

The `git` processes git-sync starts are also constrained:

- They get a minimal, explicit environment: not the daemon's own. The system and global git
  configuration are disabled, no credential helper or askpass is used, terminal prompts are off,
  and an empty temporary home directory is used (also as git's temporary directory). Only `PATH`
  and the proxy and CA settings (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` and their lower-case
  forms, `SSL_CERT_FILE`, `SSL_CERT_DIR`, `GIT_SSL_CAINFO`) are passed through from the container
  environment, so an ambient setting cannot disable TLS verification or redirect a request.
  git-sync never disables certificate verification.
- TLS certificates are verified. A custom CA bundle can be added (see
  [Trusting an internal CA for git](git-sync.md#trusting-an-internal-ca-for-git)).
- Each fetch has a fixed 30-minute limit. A push has a 30-minute limit unless the platform sets
  another (60 seconds to 24 hours), and at most four repositories sync at once unless the platform
  sets another number (1 to 32); see
  [Limits the platform can send](git-sync.md#limits-the-platform-can-send).
- Each git process runs in its own process group and is killed on timeout or shutdown, so none
  holding a credential outlives the daemon.
- A platform-supplied repository ID is validated and never used as a file path or git argument;
  the mirror directory name is a hash of it. URLs are passed to git after `--`.

## Network policy

On Kubernetes the chart creates a `NetworkPolicy` for the git-sync pods (`gitSync.networkPolicy.enabled`,
on by default whenever git-sync is enabled) that **denies all ingress**. It requires a CNI plugin
that enforces `NetworkPolicy`.

Egress is **not restricted by default**, because a `NetworkPolicy` can only match IP addresses, and
the destinations above are host names whose addresses can change. You can opt in:

```yaml
gitSync:
  networkPolicy:
    egress:
      cidrs: ["203.0.113.0/24", "198.51.100.10/32"]   # your own ranges
      ports: [443]
```

With `cidrs` set, git-sync pods may reach only those CIDRs on those TCP ports, plus DNS (port 53)
to the cluster DNS namespace (`gitSync.networkPolicy.egress.dnsNamespace`, default `kube-system`).
The list must cover the gateway, the replica, and every git server, and you must keep it current if
their addresses change. A wrong list stops syncing rather than allowing extra traffic. A replica
host pinned with a port other than 443 (`host:port`), or a git server on another port, needs that
port added to `ports`.

Limits to be aware of:

- A `NetworkPolicy` **cannot match host names**. For a hostname allow-list, use your perimeter
  firewall or an egress proxy.
- **Restricting egress by host name is your responsibility.** Allow outbound 443 from the
  git-sync host or pods to the gateway host, the replica host, and your git server(s) only. The
  host names for the first two come from your installation instructions; the third is the set of
  base URLs you configure.
- The DNS rule allows port 53 to the pods of the cluster DNS namespace. It does not match a
  node-local DNS cache (such as NodeLocal DNSCache), which listens on a node address; if you run
  one, add that address to `cidrs` or leave egress unrestricted and filter at the perimeter.
- The policy does not cover image pulls, which are made by the node's container runtime. Allow the
  registry from the nodes.

## Supply chain

- **What you run.** The git-sync image is built from [`Dockerfile.gitsync`](../Dockerfile.gitsync) by
  this repository's release workflow, from a release tag. It holds the `mkonnect-gitsync` binary
  (built with `CGO_ENABLED=0`), `git` and CA certificates on an Alpine base, and runs as uid 65532;
  the package manager is removed in the build. The runtime Alpine base is pinned by digest (the Go
  builder stage, which does not ship in the image, is pinned by tag only); `git` and
  `ca-certificates` are resolved from Alpine's package index at build time, so the image as a whole
  is not bit-for-bit reproducible, and the build fails if `git` is older than 2.31.
- **Pin by digest.** Each release records the image digests in `release-metadata.json`. Use the
  digest, not a tag, when you install (`gitSync.image.digest` in the chart; `repository@sha256:...` with
  Docker). A release can exist without a git-sync image; check as described in
  [Check that the release has a git-sync image](git-sync.md#check-that-the-release-has-a-git-sync-image).
- **Signatures and provenance.** The release workflow is set up to sign each released image
  keylessly with [Sigstore](https://docs.sigstore.dev/cosign/signing/overview/) cosign, using the
  workflow's own identity, and to attest build provenance, which GitHub stores. You verify
  both yourself, from any machine, with the commands in
  [Verify the image signature](git-sync.md#verify-the-image-signature) and
  [RELEASING.md](../RELEASING.md#image-signatures-and-provenance). git-sync never checks them. Keyless
  signatures are recorded in Sigstore's public transparency log.
- **Releases up to and including 20261022.0 are not signed.** The signing step was added after
  that release, whose workflow does not contain it, so it has no signature and no provenance
  attestation. Verifying it, or any earlier release, fails because there is nothing to verify, not
  because it was tampered with. Signing is also best effort: it is a separate job that cannot block
  or undo a release, and whether the public registry accepts the signature artifacts has not been
  confirmed (see [RELEASING.md](../RELEASING.md#image-signatures-and-provenance)). So a signed image
  is not guaranteed even from a later release. Verify the digest you will run, and treat a failed
  verification as "unsigned", not as proof of tampering and not as a clean bill of health.
- **What is not signed.** Signing and provenance cover the container images. The Helm chart has
  neither; each release publishes it with a SHA-256 checksum file, and you can read its templates
  in [`charts/mkonnect`](../charts/mkonnect).
- **Code checks.** This repository's CI runs the Go tests with the race detector, `staticcheck` and
  `govulncheck` on changes.

## Use a least-privilege git account

git-sync replicates **every branch and tag** of each repository you select, so decide what to
select, then give it only that:

- Create a **dedicated service account** for git-sync; do not reuse a person's credentials.
- Grant it **read access to the intended repositories only**. Read-only is all that is needed;
  git-sync never pushes to your git server. Prefer a token limited to read scopes.
- Use a token with an **expiry** and rotate it on a schedule. To rotate, update the
  provider configuration: git-sync applies it within seconds of the file changing, stops any sync still using the old
  credential and syncs again with the new one, so the old token can be revoked on the git server as
  soon as git-sync's log shows the reload (on Kubernetes, once the updated Secret has reached the
  pod; revoking earlier only makes syncs fail until it does; see [Reloading the provider configuration](git-sync.md#reloading-the-provider-configuration)).
- Remember that a repository's history may contain material that is no longer in its current
  files. Everything reachable from a replicated branch or tag is copied.
- On Kubernetes, supply the credential from a Secret you manage, ideally backed by your secret
  manager, rather than typing it into the pod.
- Encrypt the data volume, and restrict who can read it or take snapshots of it: it holds a full
  copy of the replicated repositories, and the signing key.

## Uninstalling

Stopping git-sync ends all access at once: it makes no connection once stopped. To remove it completely,
and to revoke the credentials it held, follow
[Uninstalling](git-sync.md#uninstalling) in the git-sync guide: it covers Docker and Helm, what to
delete (volumes, Secrets, host files), revoking your git server credential, and what remains on the
Memento side.
