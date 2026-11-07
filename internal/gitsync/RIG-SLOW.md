# The slow replica rig

A manual test rig for the git-sync connector. It runs scenarios that are too heavy for every pull
request (a large first push, thousands of refs and deletions, a request-size limit, a slow replica, the
repository ceiling) through the connector's real sync path: `syncRepo` with real `git` fetching from an
origin over HTTP and pushing to a replica over TLS. It is built only with `-tags rig` and is never part
of `go test ./...`, the pull-request gate or any released artifact.

The replica is **stock `git http-backend`** behind a small TLS front end: the shared replica harness
of the fast rig (`replicarig_test.go`, described in [RIG.md](RIG.md)), which the slow rig builds on
instead of making a second one. The slow rig uses its push throttle (`SetPushThrottle`) and its record
of each push request's size. The rig therefore proves the **connector's** side only. See
[What the rig does not prove](#what-the-rig-does-not-prove).

## Run it

```sh
GOTOOLCHAIN=go1.27.3 go test -tags rig -run '^TestRigSlow' -timeout 60m -v ./internal/gitsync
```

`-timeout 60m` (or more) is required: the default of ten minutes ends the run half way. The workflow
uses 80 minutes. A scenario that finds
less time than it needs left stops at once and says so. Run one scenario with its name instead of the
pattern: `TestRigSlowLargeRepo`, `TestRigSlowManyRefs`, `TestRigSlowPushTimeout`. The steps inside a
scenario share state and run in order, so a scenario is always run whole.

A manual GitHub run is available as the **Slow replica rig** workflow (Actions tab, "Run workflow"),
with inputs for the sizes below. It runs the same command on a hosted runner, has no schedule and uses
no secrets.

### How long it takes

On a recent laptop, at the defaults: `TestRigSlowLargeRepo` under a minute, `TestRigSlowPushTimeout`
about 2.5 minutes (it waits out a 60 second timeout and a 90 second push), and `TestRigSlowManyRefs`
about 10 minutes, nearly all of it git pushing thousands of refs one request at a time (each step of
the request-size sweep is a batched deletion sync of about a minute). Plan for 15 to 20 minutes
in total. Halving `RIG_REFS` roughly halves the many-refs time.

### Prerequisites

- **git 2.31 or newer with `git-http-backend`** (it ships with git; `$(git --exec-path)/git-http-backend`
  must exist). The rig skips, naming what is missing, if either is absent.
- **Go**, the toolchain the repository pins.
- **Free disk** in the temporary directory (`TMPDIR`): about four times `RIG_LARGE_MIB` for the large
  scenario (origin, mirror, replica and the replica's pack), about 2 GiB for the others. The rig checks
  this first and stops if the space is not there.
- **Memory**: the front end holds one push in memory while it measures it, so budget about twice
  `RIG_LARGE_MIB` plus the connector's git processes.
- **Unix** (macOS or Linux). The connector runs git in its own process group.
- Optional: `/usr/bin/time` (GNU `-f %M`, or BSD/macOS `-l`) for the peak-memory figure. Without it the
  figure reads `n/a`. `RIG_RSS=0` turns it off.

Everything stays on localhost. No network, account, credential or cloud service is involved; the
tokens are random values made for the run.

### Sizes

| Variable | Default | Range | Meaning |
|---|---|---|---|
| `RIG_LARGE_MIB` | `512` | 1 to 65536 | Size of the large scenario's origin: random (incompressible) blobs over up to 200 commits |
| `RIG_REFS` | `10000` | 200 to 200000 | Branches and tags in the many-refs scenario (half and half) |
| `RIG_BATCH_REFS` | `1000` | 100 to 100000 | `push_batch_refs` of the batched first push. The platform accepts 100 to 100000 |
| `RIG_DELETE_PERCENT` | `90` | 1 to 99 | Share of the refs the origin drops before the deletion steps. What remains must be at least 400 refs |
| `RIG_SLOW_PUSH_SECONDS` | `90` | 70 to 3600 | How long the throttled replica takes over one push |
| `RIG_RSS` | on | `0` | `0` disables the peak-memory measurement |
| `RIG_KEEP` | off | any value | Keeps the generated repositories in a directory named in the log, for inspection |

A value outside its range stops the run with a message. Limits are built with the connector's own
`ResolveLimits`, so units and validation are exactly what a platform-supplied configuration gets.

## Scenarios

**`TestRigSlowLargeRepo`** (a, e). Generates an origin of `RIG_LARGE_MIB` MiB with `git fast-import`
(nothing is checked in), then:

1. a first sync into an empty replica: fetch, then one push;
2. a second sync that finds nothing to do;
3. the repository ceiling (`max_repository_mb`): a fresh mirror with a 1 MiB ceiling downloads the
   origin and refuses to push; the ceiling is then set to the largest whole number of MiB *below* the
   mirror's size (measured with the same walk the connector uses), which must still refuse; one MiB
   higher must push.

**`TestRigSlowManyRefs`** (b, c). Generates `RIG_REFS` branches and tags, then:

1. a first push without batching (one push) and with `push_batch_refs` (a plan, batch pushes, one
   final push);
2. a resync of each, which must write nothing;
3. the origin drops `RIG_DELETE_PERCENT` percent of the refs and moves every other one. The same
   change is synced unbatched, and batched with a batch of a quarter of the remaining refs. Either
   way all deletions travel in the one final `--prune` push. In the batched run, after every batch push
   the replica is checked: no ref it held may be gone before the final push;
4. a request-size sweep: the replica front end refuses (413) a push whose body exceeds a limit, and the
   batched deletion sync is repeated from the same converged replica for a series of limits around the
   size of the final push. It reports, per limit, the result and the error type.

**`TestRigSlowPushTimeout`** (d). The replica front end reads a push at a throttled rate so that an
8 MiB push takes `RIG_SLOW_PUSH_SECONDS`. With `push_timeout_seconds` at its minimum, 60, the push is cut
at 60 seconds and reported as `push_timeout`, leaving the replica empty. The replica then speeds up and
the next sync converges. With `push_timeout_seconds` at `RIG_SLOW_PUSH_SECONDS` plus 60, the same slow
push completes. This is the configured value being honoured, not the 30 minute default.

## Reading the result

Every scenario prints lines that start with `RIG-SUMMARY`, one per step, as `key=value` pairs, and ends
with `RIG-RESULT <test> PASS` or `FAIL`. Timings, the largest size the mirror reached while the sync
ran (`peak_mirror_mib`), the number of refs pushed, the pushes made (`pushes=plan,batch*4,final`), the
largest request body and the peak memory (`peak_rss_mib`, the largest resident set of any single
process in the tree of a git call the connector made, as reported by `time`) are all in them. Read the memory figure with
care: it counts the pack files git maps into memory, which the operating system can reclaim, so it
tracks the repository's size on the large scenario without meaning that git needs that much heap. Copy those lines into the
report.

### What a PASS is

All of these hold, and each is asserted (a failed assertion fails the test and names the step):

- after every sync that must succeed, the replica's branches and tags are **exactly the origin's**
  (name and object id), and `git fsck --connectivity-only` finds every object they need (large scenario);
- the number of refs pushed equals the number that changed;
- the pushes made have the expected shape: unbatched is exactly `final`; batched is `plan`, the number of
  batches the package's own partitioning gives, then `final`; no batch uses `--prune` or deletes, no
  batch holds more than `push_batch_refs` refs or more than 64 KiB of refspecs, and together the
  batches carry exactly the refs that changed;
- **deletions converge in one run, in exactly one `--prune` push** (`converged_in_one_push=true`), and no
  ref the replica held is gone before that push (`no_replica_ref_lost_before_final=true`);
- a limit at or above the largest push request of the run never refuses the sync; a limit well below it
  always does, as `replica_rejected` with the limit actually having refused a request, and a refused run
  deletes nothing it was not meant to;
- the push timeout cuts the push at about the configured value (59 to 80 seconds for 60), classifies it
  as `push_timeout`, leaves the replica as it was, and the slow push under a longer timeout lasts at
  least 80% of `RIG_SLOW_PUSH_SECONDS`, so the throttle really held;
- a ceiling below the mirror's size gives `repository_over_limit`, makes **no** push, keeps the mirror
  and leaves the replica empty; a ceiling above it pushes;
- **no credential** appears in the rig's output, in git's own stderr or in a sync result, and none in
  what the connector logs (which is little: one debug line per sync, so that last check is a weak one).

### What a FAIL means, and what to report

A failure is either the connector or the rig. First check the line of the failing step. Then report, in
a new public issue with a synthetic reproduction and the `RIG-SUMMARY` lines (no hostnames, no real
repository names):

- a replica that differs from the origin, or objects missing from it: a correctness bug in the sync;
- more than one prune push, or a ref gone before the final push: a break of the batching safety rule
  documented in `pushbatch.go`;
- a sync that does not converge although every limit is above the final push: a bug;
- a push that outlives `push_timeout_seconds`, or is cut earlier: the limit is not honoured;
- a mirror that is pushed although over the ceiling, or discarded after refusing: a bug in the ceiling;
- a credential anywhere: a security bug (report it privately, see the security policy);
- the same step failing only at large sizes: report the size, the peak memory and the time.

Limits found by the rig that are not bugs are results too: the size of the final prune push per
deletion (`bytes_per_deletion`) and the request-size limit below which a mass deletion cannot be
carried at all (`largest_refused_limit`, `smallest_accepted_limit`). Deletions are not batched by
design, so a replica that limits request bodies below that size cannot take such a sync whatever
`push_batch_refs` is. Pass those numbers on to whoever runs the replica.

## What the rig does not prove

The replica service in production is not stock `git http-backend`. It may differ in how it
authenticates, in the size, quota and ref-count limits it enforces, in the timeouts of the load
balancer in front of it, and in how it answers when it refuses. The rig shows what the **connector**
does against a standards-following git server and what its own limits do. It does not show that the
real replica accepts a given push. Anything the real replica does is outside it.

Two properties of the stand-in are the rig's and not the connector's:

- git sends a push over 1 MiB with a chunked body, which Go's CGI handler cannot pass to
  `http-backend`; the front end buffers the whole request and states its length, as a load balancer
  would. That makes the front end's memory grow with the push, and without a body limit it refuses a
  push over 64 MiB, so the rig gives every front a limit too large to apply.
- A refused (413) request is answered after the front end has read the body (up to 16 MiB of it), so
  git reports the status. A real proxy may answer before reading the body, and git may then report a
  broken connection instead, which the connector classifies differently. The sweep asserts that every
  refused step failed as `replica_rejected`, which holds for this front end only.

## Packaging

The rig is the `replicarig_slow_*_test.go` files under the `rig` build tag, plus this file and a
manually triggered workflow. Their names are covered by the artifact content check
(`scripts/check-artifact-contents.sh`), which fails the build if a test, a rig file or this document
is found in an image, the chart package or the release assets; CI also compiles and lints the tagged
files (without running them) so they do not rot. Nothing of it is compiled into the connector or the git-sync image, nothing is in the Helm
chart or the release assets, and the files it generates live in a temporary directory removed at the
end of the run. `go build`, `go test ./...`, `go vet ./...` and `staticcheck ./...` without the tag do
not see it; `go vet -tags rig ./internal/gitsync` compiles it.
