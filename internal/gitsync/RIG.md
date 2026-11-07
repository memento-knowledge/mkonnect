# The replica rig (fast)

Test-only. The rig is in `replicarig_test.go` (the harness) and `replicarig_scenarios_test.go` (the
scenarios). Both are `_test.go` files: they are compiled into the test binary only and are kept out of
the container images, the Helm chart and the release assets (`.dockerignore`, `charts/mkonnect/.helmignore`
and `scripts/check-artifact-contents.sh`, run by CI).

It runs in the normal gate (`go test ./... -race`) and skips itself when `git` or `git-http-backend` is
missing.

## What it is

`replicaFront` is a TLS server (a throwaway CA from `internal/testca`) in front of a stock
`git http-backend` that serves one bare repository, the "replica". It stands in for a load balancer
in front of the replica. The connector trusts its CA through `GIT_SSL_CAINFO`, which is how the daemon
trusts an internal CA. `replicaRig` wires it to `batchEnv` (origin and replica on disk, a recorder
around the real git runner, injectable limits and clock), so a scenario drives the real `syncRepo`
with the real git binary, and the strings the classifier sees are what git prints.

The knobs are documented at the top of `replicarig_test.go`: the Bearer token (rotatable), a request
body limit that answers 413, a stall, an immediate error status, and a switch that turns the replica's
authentication into 401 mid-run (after n completed pushes, or only for the POST of a push).

## What it proves, and what it does not

The rig proves **the connector's side**: given a replica that answers like this, the connector
pushes the right refs in the right order, deletes nothing before every batch succeeded, reports the
right typed outcome (`replica_rejected`, `push_timeout`, `credential_rejected`, `replica_unreachable`),
keeps the mirror, and never lets a credential out of the places it is meant to be.

It does **not** prove how the real replica service behaves. Treat every difference below as untested:

- **Authentication.** The rig accepts one Bearer token and answers a bare `401` (no
  `WWW-Authenticate`). The real service may use other headers, other statuses (`403`), token scopes,
  or expiry rules of its own.
- **Size and quota limits.** The rig refuses a push body over a fixed size with `413`, after reading
  it. A real front end may reset the connection instead, answer before the client finished sending,
  limit a repository's total size, or refuse a push for a reason that is only visible in git's own
  report. The connector classifies what git reports, so a reset would not read as `replica_rejected` (not exercised here).
- **Timeouts.** The rig stalls forever, or answers an error at once. A real load balancer has idle
  and response timeouts of its own and cuts a slow push at a time the connector does not control.
- **Ref counts and pack sizes.** The rig uses a few hundred refs and kilobyte-sized packs. Real
  repositories are larger; the service may limit refs per push or per repository.
- **Protocol.** The rig speaks HTTP/1.1 only (the test server does not offer HTTP/2) and buffers every
  push body before forwarding it, as a request-buffering proxy would. The stock backend behind it
  allows pushes for any authenticated caller and has no hooks.
- **Concurrency.** One connector, one repository at a time.

If the real service differs on one of these, the rig is where to reproduce the connector's reaction:
add a knob to `replicaFront` and a scenario.
