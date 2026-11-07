//go:build rig

package gitsync

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// The slow rig. These scenarios run the connector's real sync path (fetch from an origin, push to a
// replica) with real git against stock `git http-backend`, at sizes too heavy for every pull request.
// They are built only with `-tags rig` and are run by hand (see RIG-SLOW.md). Test names start
// with TestRigSlow.
//
// The replica here is stock git: it proves the connector's side only. A real replica service may
// differ in how it authenticates, sizes and times out requests, and in how many refs it accepts.

// requireRSS returns the RSS observer, or nil (and a log line) when it cannot be used.
func requireRSS(t *testing.T) *rigRSS {
	t.Helper()
	if os.Getenv("RIG_RSS") == "0" {
		t.Log("RIG peak RSS: skipped (RIG_RSS=0)")
		return nil
	}
	return newRigRSS(t)
}

// (a) A large first push, and (e) the repository ceiling.
func TestRigSlowLargeRepo(t *testing.T) {
	rigPrepare(t, 20*time.Minute)
	k := rigKnobsFromEnv(t)
	w := newRigWorld(t)

	start := time.Now()
	written := genLargeOrigin(t, w.originDir, k.LargeMiB, min(200, max(10, k.LargeMiB/2)))
	want := refMap(t, w.originDir)
	w.report.note("generated an origin of %.0f MiB of random blobs in %v (%d refs)", mib(written), time.Since(start).Round(time.Millisecond), len(want))

	// First push into an empty replica.
	site := w.site("large", "m-large")
	out := site.sync(rigLimits(t, 0, 0, 0), rigSyncOpts{rss: requireRSS(t)})
	out.mustBeOK(t, "first sync")
	site.requireReplicaMatches(want, "after the first sync")
	if got := out.refsPushed(); got != len(want) {
		t.Errorf("refs pushed = %d, want %d", got, len(want))
	}
	if n := out.count("final"); n != 1 || len(out.pushes()) != 1 {
		t.Errorf("pushes = %s, want exactly one final push", out.seq())
	}
	fsck := site.fsckConnectivity()
	mirrorBytes := dirSize(site.mirrorDir())
	replicaBytes := dirSize(site.replicaDir)
	w.report.sum("large-first-push", "origin_mib", fmt.Sprintf("%.0f", mib(written)), "total", out.elapsed.Round(time.Second),
		"fetch", out.dur("fetch").Round(time.Second), "push", out.dur("final").Round(time.Second),
		"peak_mirror_mib", fmt.Sprintf("%.0f", out.peakMiB), "final_mirror_mib", fmt.Sprintf("%.0f", mib(mirrorBytes)),
		"replica_mib", fmt.Sprintf("%.0f", mib(replicaBytes)), "refs_pushed", out.refsPushed(),
		"replica_fsck", fsck.Round(time.Millisecond), "peak_rss_mib", rssText(out), "replica_equals_origin", true)

	// A second sync finds nothing to do.
	again := site.sync(rigLimits(t, 0, 0, 0), rigSyncOpts{})
	again.mustBeOK(t, "second sync")
	site.requireReplicaMatches(want, "after the second sync")
	if again.refsPushed() != 0 {
		t.Errorf("second sync pushed %d refs, want 0", again.refsPushed())
	}
	w.report.sum("large-resync", "total", again.elapsed.Round(time.Millisecond), "refs_pushed", again.refsPushed())

	// (e) The ceiling. A fresh mirror, a 1 MiB ceiling: the origin is downloaded in full, nothing is
	// pushed, and the mirror is kept.
	over := w.site("large-over", "m-over")
	first := over.sync(rigLimits(t, 0, 0, 1), rigSyncOpts{})
	if first.res.ErrorType != ErrRepositoryOverLimit {
		t.Fatalf("1 MiB ceiling: sync = %s, want error/%s", first.verdict(), ErrRepositoryOverLimit)
	}
	first.requireNoPushes(t, "1 MiB ceiling")
	if _, err := os.Stat(over.mirrorDir()); err != nil {
		t.Fatalf("the mirror was not kept after repository_over_limit: %v", err)
	}
	size := dirSize(over.mirrorDir()) // the same measure the ceiling check uses
	// The largest whole-MiB ceiling below the size, and the smallest above it. A fetch rewrites a few
	// small files (FETCH_HEAD), so a size within 16 KiB of a boundary could tip either check: step one
	// more MiB away in that case.
	const margin = 16 << 10
	tight := size / mebibyte
	if size-tight*mebibyte < margin {
		tight--
	}
	loose := tight + 1
	if loose*mebibyte-size < margin {
		loose++
	}
	if tight < 1 {
		t.Fatalf("the mirror is only %.2f MiB: raise RIG_LARGE_MIB", mib(size))
	}
	below := over.sync(rigLimits(t, 0, 0, int(tight)), rigSyncOpts{})
	if below.res.ErrorType != ErrRepositoryOverLimit {
		t.Fatalf("ceiling %d MiB just below the %.2f MiB mirror: sync = %s, want error/%s", tight, mib(size), below.verdict(), ErrRepositoryOverLimit)
	}
	below.requireNoPushes(t, "ceiling just below the mirror")
	if refs := refMap(t, over.replicaDir); len(refs) != 0 {
		t.Fatalf("the replica holds %d refs after a refused sync, want none", len(refs))
	}
	if after := dirSize(over.mirrorDir()); after != size {
		w.report.note("the mirror changed size across a refused sync: %d bytes, then %d", size, after)
	}
	at := over.sync(rigLimits(t, 0, 0, int(loose)), rigSyncOpts{})
	at.mustBeOK(t, fmt.Sprintf("ceiling %d MiB just above the mirror", loose))
	over.requireReplicaMatches(want, "after the ceiling was raised")
	w.report.sum("large-over-limit", "mirror_mib", fmt.Sprintf("%.2f", mib(size)), "refused_at_mib", tight, "accepted_at_mib", loose,
		"download_before_refusal", first.elapsed.Round(time.Second), "pushes_when_refused", 0, "mirror_kept", true,
		"replica_equals_origin_after_raise", true)
}

// (b) Thousands of refs, (c) thousands of deletions carried by the single final prune push, and where
// a request-size limit starts to refuse them. The steps share state and run in order.
func TestRigSlowManyRefs(t *testing.T) {
	rigPrepare(t, 15*time.Minute)
	k := rigKnobsFromEnv(t)
	kept := k.Refs - k.Refs*k.DeletePercent/100
	if kept < 400 {
		t.Fatalf("RIG_REFS=%d with RIG_DELETE_PERCENT=%d leaves %d refs: need at least 400 so the deletion run still batches", k.Refs, k.DeletePercent, kept)
	}
	w := newRigWorld(t)

	names := genManyRefsOrigin(t, w.originDir, k.Refs)
	before := refMap(t, w.originDir)
	w.report.note("generated an origin with %d branches and tags", len(before))

	// (b) First push, unbatched and batched, each into an empty replica.
	plain := w.site("plain", "m-plain")
	out := plain.sync(rigLimits(t, 0, 0, 0), rigSyncOpts{rss: requireRSS(t)})
	out.mustBeOK(t, "unbatched first sync")
	plain.requireReplicaMatches(before, "unbatched first sync")
	if out.seq() != "final" || out.refsPushed() != len(before) {
		t.Fatalf("unbatched first sync: pushes %q, refs pushed %d; want \"final\", %d", out.seq(), out.refsPushed(), len(before))
	}
	w.report.sum("refs-first-push-unbatched", "refs", len(before), "total", out.elapsed.Round(time.Millisecond), "fetch", out.dur("fetch").Round(time.Millisecond),
		"push", out.dur("final").Round(time.Millisecond), "request_bytes", out.pushes()[0].front.MaxBody, "peak_rss_mib", rssText(out), "replica_equals_origin", true)

	batched := w.site("batched", "m-batched")
	bout := batched.sync(rigLimits(t, k.BatchRefs, 0, 0), rigSyncOpts{rss: requireRSS(t)})
	bout.mustBeOK(t, "batched first sync")
	batched.requireReplicaMatches(before, "batched first sync")
	wantBatches := expectedBatches(namesOf(before), nil, k.BatchRefs)
	checkBatchedSeq(t, bout, wantBatches, k.BatchRefs, len(before), namesOf(before))
	if bout.refsPushed() != len(before) {
		t.Errorf("batched first sync pushed %d refs, want %d", bout.refsPushed(), len(before))
	}
	w.report.sum("refs-first-push-batched", "refs", len(before), "batch_refs", k.BatchRefs, "pushes", bout.seq(), "total", bout.elapsed.Round(time.Millisecond),
		"fetch", bout.dur("fetch").Round(time.Millisecond), "plan", bout.dur("plan").Round(time.Millisecond), "batches", bout.dur("batch").Round(time.Millisecond),
		"final", bout.dur("final").Round(time.Millisecond), "peak_rss_mib", rssText(bout), "replica_equals_origin", true)

	// A later sync of a converged replica is cheap in both modes.
	for _, c := range []struct {
		name string
		site *rigSite
		lim  Limits
	}{{"unbatched", plain, rigLimits(t, 0, 0, 0)}, {"batched", batched, rigLimits(t, k.BatchRefs, 0, 0)}} {
		again := c.site.sync(c.lim, rigSyncOpts{})
		again.mustBeOK(t, c.name+" resync")
		if again.refsPushed() != 0 || again.count("batch") != 0 {
			t.Errorf("%s resync: pushes %q, refs pushed %d; want no refs written", c.name, again.seq(), again.refsPushed())
		}
		w.report.sum("refs-resync-"+c.name, "total", again.elapsed.Round(time.Millisecond), "pushes", again.seq(), "refs_pushed", again.refsPushed())
	}

	// (c) The origin drops most of its refs and moves every ref it keeps, so the next sync both
	// writes and deletes, and the mirror stays above the batch size.
	snapReplica, snapMirror := plain.snapshot("converged")
	dropped, moved := dropAndMoveRefs(t, w.originDir, names, k.DeletePercent)
	after := refMap(t, w.originDir)
	if len(dropped) == 0 || len(moved) < 400 {
		t.Fatalf("dropped %d and moved %d refs", len(dropped), len(moved))
	}
	w.report.note("the origin dropped %d refs and moved %d; %d remain", len(dropped), len(moved), len(after))
	pre := refMap(t, plain.replicaDir)

	del := w.site("delete-plain", "m-delete-plain")
	del.restore(snapReplica, snapMirror)
	dout := del.sync(rigLimits(t, 0, 0, 0), rigSyncOpts{})
	dout.mustBeOK(t, "unbatched deletion sync")
	del.requireReplicaMatches(after, "unbatched deletion sync")
	if dout.seq() != "final" || dout.refsPushed() != len(dropped)+len(moved) {
		t.Fatalf("unbatched deletion sync: pushes %q, refs pushed %d; want \"final\", %d", dout.seq(), dout.refsPushed(), len(dropped)+len(moved))
	}
	if dout.pushes()[0].front.Posts == 0 {
		t.Fatal("the front end never saw the deletion push: the run proved nothing")
	}
	w.report.sum("deletions-unbatched", "deleted", len(dropped), "moved", len(moved), "pushes", dout.seq(), "total", dout.elapsed.Round(time.Millisecond),
		"fetch_prune", dout.dur("fetch").Round(time.Millisecond), "push", dout.dur("final").Round(time.Millisecond),
		"request_bytes", dout.pushes()[0].front.MaxBody, "chunked", dout.pushes()[0].front.Chunked, "converged_in_one_push", true, "replica_equals_origin", true)

	// The same, batched. Before the final push nothing may have been deleted from the replica.
	batchSize := max(100, kept/4)
	delb := w.site("delete-batched", "m-delete-batched")
	delb.restore(snapReplica, snapMirror)
	checks := 0
	hook := func(c rigCall, _ int) {
		if c.kind == "final" {
			return
		}
		checks++
		now := refMap(t, delb.replicaDir)
		for n := range pre {
			if _, ok := now[n]; !ok {
				t.Fatalf("replica ref %s is gone after a %s push, before the final push", n, c.kind)
			}
		}
	}
	dbout := delb.sync(rigLimits(t, batchSize, 0, 0), rigSyncOpts{after: hook})
	dbout.mustBeOK(t, "batched deletion sync")
	delb.requireReplicaMatches(after, "batched deletion sync")
	checkBatchedSeq(t, dbout, expectedBatches(moved, dropped, batchSize), batchSize, len(after), moved)
	if want := dbout.count("plan") + dbout.count("batch"); checks != want {
		t.Errorf("the replica was checked %d times, want once after each of the %d pushes before the final one", checks, want)
	}
	if dbout.count("final") != 1 {
		t.Fatalf("batched deletion sync: %d final pushes, want 1", dbout.count("final"))
	}
	if dbout.refsPushed() != len(dropped)+len(moved) {
		t.Errorf("batched deletion sync pushed %d refs, want %d", dbout.refsPushed(), len(dropped)+len(moved))
	}
	finalBody := dbout.pushes()[len(dbout.pushes())-1].front
	// The largest push request of the run: with few deletions a batch can be bigger than the final push.
	var largest int64
	for _, c := range dbout.pushes() {
		largest = max(largest, c.front.MaxBody)
	}
	w.report.sum("deletions-batched", "deleted", len(dropped), "moved", len(moved), "batch_refs", batchSize, "pushes", dbout.seq(),
		"total", dbout.elapsed.Round(time.Millisecond), "no_replica_ref_lost_before_final", true, "checks", checks,
		"final_request_bytes", finalBody.MaxBody, "chunked", finalBody.Chunked, "converged_in_one_push", true, "replica_equals_origin", true)

	// Where the replica's request-size limit starts to refuse the deletions. Every step starts from the
	// converged replica; a step whose replica were already current would send no request at all.
	body := largest
	limits := []int64{16 << 10, 64 << 10, body / 2, body - 1, body, 2 * body}
	slices.Sort(limits)
	limits = slices.Compact(limits)
	lastFailing, firstPassing := int64(0), int64(0)
	for _, limit := range limits {
		if limit <= 0 {
			continue
		}
		step := w.site(fmt.Sprintf("limit-%d", limit), fmt.Sprintf("m-limit-%d", limit))
		step.restore(snapReplica, snapMirror)
		step.front.SetMaxBody(limit)
		sout := step.sync(rigLimits(t, batchSize, 0, 0), rigSyncOpts{})
		step.front.SetMaxBody(rigNoBodyLimit)

		posts, refused := 0, 0
		for _, c := range sout.pushes() {
			posts += c.front.Posts
			refused += c.front.LimitStatus
		}
		got := refMap(t, step.replicaDir)
		switch {
		case sout.res.State == StateOK:
			if d := diffRefMaps(after, got); d != "" {
				t.Errorf("limit %d: sync ok but the replica differs: %s", limit, d)
			}
			if posts == 0 {
				t.Errorf("limit %d: the front end saw no push request, the step proved nothing", limit)
			}
			if firstPassing == 0 {
				firstPassing = limit
			}
		default:
			lastFailing = limit
			if refused == 0 || sout.res.ErrorType != ErrReplicaRejected {
				t.Errorf("limit %d: the sync failed as %s with %d request(s) refused by the limit, want %s from a refusal", limit, sout.verdict(), refused, ErrReplicaRejected)
			}
			// A refused run must delete nothing: it stops before, or inside, the final push.
			for n := range pre {
				if _, ok := got[n]; !ok && !slices.Contains(dropped, n) {
					t.Errorf("limit %d: a ref that was not dropped is gone from the replica: %s", limit, n)
				}
			}
		}
		w.report.sum("deletions-413-sweep", "limit_bytes", limit, "result", sout.verdict(), "pushes", sout.seq(), "limit_refusals", refused,
			"push_requests", posts, "elapsed", sout.elapsed.Round(time.Millisecond))
		if limit >= body && sout.res.State != StateOK {
			t.Errorf("limit %d is not below the %d-byte largest push but the sync was refused: %s", limit, body, sout.verdict())
		}
		if limit < body/2 && sout.res.State == StateOK {
			t.Errorf("limit %d is far below the %d-byte largest push but the sync passed", limit, body)
		}
	}
	w.report.sum("deletions-413-where", "largest_push_bytes", body, "final_push_bytes", finalBody.MaxBody, "bytes_per_deletion", fmt.Sprintf("%.0f", float64(finalBody.MaxBody)/float64(len(dropped))),
		"largest_refused_limit", lastFailing, "smallest_accepted_limit", firstPassing)
}

// (d) The effective push timeout, on a replica that reads a push slowly.
func TestRigSlowPushTimeout(t *testing.T) {
	rigPrepare(t, 10*time.Minute)
	k := rigKnobsFromEnv(t)
	w := newRigWorld(t)

	const payloadMiB = 8
	genLargeOrigin(t, w.originDir, payloadMiB, 8)
	want := refMap(t, w.originDir)
	// The pack is the blobs plus a little, so this rate makes the push last about SlowPushSecs.
	rate := int64(payloadMiB) * mebibyte / int64(k.SlowPushSecs)
	a := w.site("slow-a", "m-slow")
	a.front.SetPushThrottle(rate)
	w.report.note("the replica reads a push at %d bytes/s: about %ds for %d MiB", rate, k.SlowPushSecs, payloadMiB)

	// push_timeout_seconds at its minimum (60) is shorter than the push: it must be the limit that cuts it.
	lim := rigLimits(t, 0, 60, 0)
	if lim.PushTimeout != 60*time.Second {
		t.Fatalf("effective push timeout = %v, want 60s", lim.PushTimeout)
	}
	cut := a.sync(lim, rigSyncOpts{timeout: 10 * time.Minute})
	if cut.res.State != StateError || cut.res.ErrorType != ErrPushTimeout {
		t.Fatalf("60s push timeout on a %ds push: sync = %s, want error/%s", k.SlowPushSecs, cut.verdict(), ErrPushTimeout)
	}
	pushDur := cut.dur("final")
	// The timeout, then SIGTERM and a short grace: well inside 20s on any machine, and never early.
	if pushDur < 59*time.Second || pushDur > 80*time.Second {
		t.Errorf("the push was cut after %v, want about 60s", pushDur)
	}
	if refs := refMap(t, a.replicaDir); len(refs) != 0 {
		t.Errorf("the replica holds %d refs after a timed-out push, want none", len(refs))
	}
	w.report.sum("push-timeout-cut", "push_timeout_seconds", 60, "slow_push_seconds", k.SlowPushSecs, "result", cut.verdict(),
		"push_lasted", pushDur.Round(time.Second), "replica_refs_after", len(refMap(t, a.replicaDir)))

	// The replica recovers once it is fast again.
	a.front.SetPushThrottle(0)
	rec := a.sync(lim, rigSyncOpts{})
	rec.mustBeOK(t, "sync after the replica sped up")
	a.requireReplicaMatches(want, "after recovery")
	w.report.sum("push-timeout-recovery", "result", rec.verdict(), "total", rec.elapsed.Round(time.Millisecond), "replica_equals_origin", true)

	// A longer push_timeout_seconds lets the same slow push finish.
	long := rigLimits(t, 0, k.SlowPushSecs+60, 0)
	if long.PushTimeout != time.Duration(k.SlowPushSecs+60)*time.Second {
		t.Fatalf("effective push timeout = %v", long.PushTimeout)
	}
	b := w.site("slow-b", "m-slow")
	b.front.SetPushThrottle(rate)
	ok := b.sync(long, rigSyncOpts{timeout: 10 * time.Minute})
	ok.mustBeOK(t, "slow push under a long timeout")
	b.requireReplicaMatches(want, "after the slow push")
	if got := ok.dur("final"); got < time.Duration(float64(k.SlowPushSecs)*0.8*float64(time.Second)) {
		t.Errorf("the slow push lasted only %v: the throttle did not hold, the run proved nothing", got)
	}
	w.report.sum("push-timeout-honoured", "push_timeout_seconds", k.SlowPushSecs+60, "push_lasted", ok.dur("final").Round(time.Second),
		"longer_than_minimum_timeout", ok.dur("final") > 60*time.Second, "result", ok.verdict(), "replica_equals_origin", true)
}

// rssText renders a sync's peak RSS, or why there is none.
func rssText(o rigOutcome) string {
	if o.rssMiB == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f", o.rssMiB)
}

func namesOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// dropAndMoveRefs makes the origin drop percent percent of the generated refs and move every other one
// to a different commit. It returns the dropped and the moved ref names.
func dropAndMoveRefs(t *testing.T, gitDir string, generated []string, percent int) (dropped, moved []string) {
	t.Helper()
	commits := strings.Fields(git(t, "", "--git-dir="+gitDir, "rev-list", "refs/heads/main"))
	cur := refMap(t, gitDir)
	var b strings.Builder
	b.WriteString("start\n")
	for i, n := range generated {
		if i%100 < percent {
			dropped = append(dropped, n)
			b.WriteString("delete " + n + "\n")
			continue
		}
		next := commits[(slices.Index(commits, cur[n])+7)%len(commits)]
		moved = append(moved, n)
		b.WriteString("update " + n + " " + next + "\n")
	}
	b.WriteString("prepare\ncommit\n")
	gitStdin(t, "", b.String(), "--git-dir="+gitDir, "update-ref", "--stdin")
	git(t, "", "--git-dir="+gitDir, "pack-refs", "--all")
	slices.Sort(dropped)
	slices.Sort(moved)
	return dropped, moved
}

// expectedBatches is how many batch pushes the algorithm cuts upserts into, computed with the
// package's own splitting and partitioning (the refspec text bound included).
func expectedBatches(upserts, deletes []string, batchRefs int) int {
	if len(upserts) <= batchRefs {
		return 0
	}
	free, _ := splitConflicts(upserts, deletes, nil)
	return len(partitionBatches(free, batchRefs, maxBatchArgBytes))
}

// checkBatchedSeq asserts the shape of a batched run: a plan (when the mirror holds more refs than a batch), wantBatches batch pushes, one final push
// that prunes; no batch prunes or deletes; each holds at most batchRefs refs; and together the batches
// carry exactly upserts.
func checkBatchedSeq(t *testing.T, o rigOutcome, wantBatches, batchRefs, mirrorRefs int, upserts []string) {
	t.Helper()
	// The connector plans only when the mirror holds more refs than one batch (mirrorMayExceedBatch).
	wantSeq := "final"
	if mirrorRefs > batchRefs {
		wantSeq = "plan," + wantSeq
	}
	if wantBatches == 1 {
		wantSeq = strings.Replace(wantSeq, "final", "batch,final", 1)
	} else if wantBatches > 1 {
		wantSeq = strings.Replace(wantSeq, "final", fmt.Sprintf("batch*%d,final", wantBatches), 1)
	}
	if o.seq() != wantSeq {
		t.Fatalf("pushes = %q, want %q", o.seq(), wantSeq)
	}
	var carried []string
	for _, c := range o.calls {
		if c.kind != "batch" {
			continue
		}
		if c.prune {
			t.Errorf("a batch push used --prune")
		}
		if len(c.specs) > batchRefs {
			t.Errorf("a batch carries %d refs, want at most %d", len(c.specs), batchRefs)
		}
		for _, s := range c.specs {
			spec, ok := strings.CutPrefix(s, "+")
			from, to, _ := strings.Cut(spec, ":")
			if !ok || from == "" || from != to {
				t.Errorf("a batch refspec is not a plain +ref:ref update: %q", s)
				continue
			}
			carried = append(carried, from)
		}
		if size := specBytes(c.specs); size > maxBatchArgBytes {
			t.Errorf("a batch's refspec text is %d bytes, over the %d bound", size, maxBatchArgBytes)
		}
	}
	if wantBatches > 0 {
		slices.Sort(carried)
		if !slices.Equal(carried, upserts) {
			t.Errorf("the batches carried %d refs, want exactly the %d that changed", len(carried), len(upserts))
		}
	}
}

func specBytes(specs []string) int {
	n := 0
	for _, s := range specs {
		n += len(s) + 1
	}
	return n
}
