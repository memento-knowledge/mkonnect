# git-sync: security and data handling

This page is for security and network reviewers evaluating git-sync before approving it. It
describes what git-sync connects to, what data it moves and stores, who holds which credential,
and how the process is isolated. For installation steps, see the [git-sync guide](git-sync.md).

It describes what the open-source code in this repository does. You can verify any statement here
against [`internal/gitsync`](../internal/gitsync) and [`cmd/mkonnect-gitsync`](../cmd/mkonnect-gitsync).

## Summary

- git-sync is **outbound-only**. It listens on no port, has no Service, and the chart denies all
  ingress to its pods.
- It **reads** from your git server and **writes** to exactly one Memento replica host, which you
  pin at install time.
- It replicates **branches and tags only** (`refs/heads/*` and `refs/tags/*`).
- Your git server credential stays on your side, is scoped to your git server's address, and is
  never sent to Memento.
- The push credential for the replica is issued by the platform, held **in memory only**, and is
  never written to disk by git-sync.
- The process runs as an unprivileged user with a read-only root filesystem and no Linux
  capabilities.

## Network destinations

| Destination | Port | Direction | Purpose |
|---|---|---|---|
| Memento Bridge Gateway (the Gateway URL in the installation instructions you were given for your tenant) | 443 (`wss://`) | outbound | Registration on first start, authentication, heartbeat and status, and receiving repository configuration. |
| Memento replica host (the replica host in the same instructions) | 443, or the port you pinned | outbound | HTTPS `git push` of branches and tags. |
| Your git server(s): the base URL(s) you configure as providers | as in each base URL (443 for `https://`) | outbound | Read-only `git ls-remote` and `git fetch`. |
| Container registry (`public.ecr.aws`) | 443 | outbound, from the container runtime or the cluster nodes | Pulling the image at install and upgrade. git-sync itself does not contact it. |
| DNS | 53 | outbound | Name resolution. |

There are no inbound connections. Exact gateway and replica host names are given in the
installation instructions for your tenant; they are not listed here because they are specific to
it. The
connector refuses to push to any host other than the pinned replica host and only ever uses a
credential against git servers that you configured (see
[How the credentials are kept to their destination](#how-the-credentials-are-kept-to-their-destination)).

If your environment requires an HTTP proxy, set `HTTPS_PROXY`, `HTTP_PROXY`, and `NO_PROXY`, and
their lower-case forms, in the container environment (the chart's `gitSync.proxy.*` values set
both cases). git-sync passes exactly these six variables on to git for its fetches and pushes; git's
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
   the sync stops with an error and **does not touch the replica**. This prevents an empty or
   misbehaving origin from deleting everything that was replicated.
3. `git push --prune --force` of the same two ref namespaces to the replica. A branch or tag that
   no longer exists on your git server is removed from the replica.

Repository data travels directly between your git server, git-sync, and the replica over HTTPS.
The gateway tunnel carries only control messages: configuration from the platform, and status
(repository IDs, states, error types) from the connector.

### What the replica receives

Only `refs/heads/*` and `refs/tags/*`, together with the objects reachable from them. That is the
complete history behind every branch and tag, including files that were later deleted. It does
**not** receive other namespaces your server may keep (for example pull or merge request refs),
Git LFS object contents (git-sync uses plain `git fetch` and `git push`), repository hooks or
settings, or anything outside the repository. Review what the repositories you select contain
before replicating them.

### What is stored, and where

| Data | Where | Notes |
|---|---|---|
| Private signing key (ML-DSA-65) | `/data/gitsync-key` on the data volume | Mode `0600`, owner-only. Issued by the gateway at registration. Losing it means re-registering. |
| Provider configuration, including your git server credential | A Secret mounted read-only at `/etc/mkonnect-gitsync-providers/`, or `/data/git-sync.json` on the data volume | In **cleartext** (not encrypted at rest by git-sync). git-sync tightens the file to owner-only (`0600`) where it can, and refuses to start with a read-only file that is group-writable or accessible by others. |
| Mirrors of the replicated repositories | `/data/mirrors` on the data volume | A full copy of the history behind every branch and tag. Treat the volume as containing your source code. The mirror of a repository the platform stops listing is deleted once it has been unlisted for seven days (see [Mirror cleanup](git-sync.md#mirror-cleanup)); `GIT_SYNC_MIRROR_GC=off` turns that off. |
| Run state | `/data/mirrors/run-state.json` on the data volume | Per repository: when the last attempt started and how long it took, when the last sync succeeded, how many refs it pushed, the newest commit time and default branch at your git server, and an unsalted SHA-256 hash of the origin and replica URLs (it does not reveal them, but could confirm a guessed pair). No credential, URL, host name, path, or error text. Mode `0600`. |
| Replica push tokens | **Memory only** | Received from the platform per repository, with an expiry, and kept only for repositories that can be synced. Not written to disk and not logged. |
| One-time registration token | A read-only file mounted from a Secret (Kubernetes) or from a dedicated Docker volume | Used once. Remove it after the connector has registered. It is never an environment variable. |
| Per-sync scratch data | `/tmp` (disk-backed) | Each sync creates a throwaway, empty home directory for git, also used for git's temporary files, and removes it afterwards. The fetched objects themselves go into the mirror, not here. |
| Logs | Standard error (`docker logs`, `kubectl logs`) | See [What is logged](#what-is-logged). |

git-sync does not modify your git server. Any change there (revoking a token, deleting a
repository) takes effect at the next sync.

### What is reported to the platform

The connector sends status: for each repository its ID, state (`pending`, `syncing`, `ok`,
`error`), an error type, and, for failures it cannot classify, a single short detail line from
which URLs, host names, and absolute paths have been removed. It also sends, for each repository,
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
| Registration token | Issued for your tenant at registration; delivered to git-sync as a file | Mounted read-only, mode `0440` in Kubernetes (group-readable through the pod's `fsGroup`); in Docker, a `0600` file owned by uid 65532 in a dedicated volume mounted read-only. git-sync refuses a token file that is accessible by others or writable by its group. Not an environment variable, so it is not shown by `docker inspect` or in the pod specification. | One time; remove after registration. |
| Signing key | Generated by the gateway at registration; stored by git-sync | Owner-only file on the data volume; the private key is never sent again. Each reconnect proves possession by signing a challenge. | Until you delete the volume. |
| Git server credential | You | Stored in the provider configuration, or in a file the configuration names (`token_file`; refused when group or others can write it or another user owns it). Used only for requests to the matching git server. On Kubernetes, prefer a Secret you manage. | Until you rotate or revoke it at your git server. |
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
- Each fetch and each push has a 30-minute limit, and at most four repositories sync at once.
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

## Use a least-privilege git account

git-sync replicates **every branch and tag** of each repository you select, so decide what to
select, then give it only that:

- Create a **dedicated service account** for git-sync; do not reuse a person's credentials.
- Grant it **read access to the intended repositories only**. Read-only is all that is needed;
  git-sync never pushes to your git server. Prefer a token limited to read scopes.
- Use a token with an **expiry** and rotate it on a schedule. Rotating requires updating the
  provider configuration and **restarting** git-sync, which reads it only at startup.
- Remember that a repository's history may contain material that is no longer in its current
  files. Everything reachable from a replicated branch or tag is copied.
- On Kubernetes, supply the credential from a Secret you manage, ideally backed by your secret
  manager, rather than typing it into the pod.
- Encrypt the data volume, and restrict who can read it or take snapshots of it: it holds a full
  copy of the replicated repositories, and the signing key.

## Uninstalling

Removing git-sync stops all access immediately: it makes no connection once stopped. To remove it
completely:

1. Stop and remove the container, or disable `gitSync.enabled`, as described in
   [Uninstalling](git-sync.md#uninstalling).
2. Delete its volumes. This destroys the signing key, the provider configuration, and every
   mirror.
3. Delete the Secrets you created for it.
4. Revoke the access token you issued on your git server.
5. Remove the git-sync connector, and its repositories, in the Memento portal. What is retained
   on the Memento side for repositories already replicated is controlled there.

Because the credential for your git server is held only on your side, revoking it at the source
completes the removal; there is nothing to revoke on Memento's side for it.
