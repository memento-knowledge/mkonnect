package gitsync

import (
	"bytes"
	"context"
	"io"
	"slices"
	"sort"
	"strings"
)

// This file is the push stage of a sync: either one push of the whole mirror, or — when the platform
// sets push_batch_refs and more refs would be sent than that — the same push split into batches.
//
// THE SAFETY RULE. `git push --prune` deletes the replica's refs that the mirror lacks, so a push that
// is cut into batches must not prune per batch: a failure part-way would leave the replica with some
// of its refs deleted and the rest not yet written. The batched path therefore
//
//  1. plans first, with `git push --dry-run --porcelain --prune`, which says exactly which refs a push
//     would create, update or delete without sending anything;
//  2. pushes only the creates and updates, in batches of at most push_batch_refs, with explicit
//     refspecs and NO --prune (an explicit push never deletes);
//  3. and only after every batch succeeded makes ONE final push of the full refspecs with --prune,
//     which carries all the deletions and writes only what no batch could (everything else is
//     already current, so it sends little).
//
// A batch that fails, a token that expires between batches, or a cancellation stops the sync before
// the final push, so such a run deletes nothing. A rejected ref in the final push can leave some of
// its deletions applied, as the unbatched push always could.
//
// push_batch_refs bounds the refs written by a batch push, not the final push: that one also carries
// every deletion (they are the job of the single final prune step, and a deletion command has no
// pack data to make a push oversized) and the few refs that cannot go in a batch (see
// splitConflicts), which are written by it.
//
// The empty-origin guard (hasPushableRefs) runs before any of this, so a mirror with no refs never
// reaches a push at all. The window between the plan and the final push is the length of the batched
// run; a branch someone else created on the replica in that window would be pruned, exactly as it
// would be by an unbatched push moments after the same fetch.

const (
	// maxBatchArgBytes bounds the refspec text of one batch push, whatever push_batch_refs is, so the
	// argument list stays far below any ARG_MAX (push_batch_refs may be 100000).
	maxBatchArgBytes = 64 << 10
	// maxPlanBytes bounds the ref names a plan retains; a plan that needs more is refused, not
	// truncated, because a partial plan cannot be pushed in full.
	maxPlanBytes = 64 << 20
	// maxPlanRefLen bounds a ref name the plan accepts: longer than any ref git can store (a path is
	// at most 4096 bytes), so a plan naming one is not a plan from git.
	maxPlanRefLen = 4096
	// maxPlanLine bounds one porcelain status line the plan reads: two names, the flag, the
	// separators and git's summary.
	maxPlanLine = 2*maxPlanRefLen + 256
	// maxBatchRefLen bounds a ref name placed in a batch's argv; a longer (but valid) one is left to
	// the final push, which names no ref at all.
	maxBatchRefLen = 1024
)

// errPlanDetail is the fixed detail reported when the plan cannot be trusted. It names nothing the
// replica sent.
const errPlanDetail = "push plan could not be read; replica left unchanged"

// pushStage sends the mirror to the replica and returns the outcome of the push as a whole; stats
// receives the number of refs the pushes changed.
func (d syncDeps) pushStage(ctx context.Context, home string, t syncTask, lim Limits, stats *syncStats) syncResult {
	var counter pushCounter
	if d.runOut != nil {
		defer func() {
			stats.RefsPushed = new(int)
			*stats.RefsPushed = counter.Refs()
		}()
	}

	var batches [][]string
	if lim.PushBatchRefs > 0 && d.runOut != nil && d.mirrorMayExceedBatch(ctx, home, t, lim.PushBatchRefs) {
		plan, res, ok := d.planPush(ctx, home, t, lim, stats)
		if !ok {
			return res
		}
		if len(plan.upserts) > lim.PushBatchRefs {
			free, _ := splitConflicts(plan.upserts, plan.deletes, plan.shadows)
			batches = partitionBatches(free, lim.PushBatchRefs, maxBatchArgBytes)
		}
	}

	for _, batch := range batches {
		specs := make([]string, len(batch))
		for i, ref := range batch {
			specs[i] = "+" + ref + ":" + ref
		}
		if res, ok := d.pushOnce(ctx, home, t, lim, stats, &counter, specs, false); !ok {
			return res
		}
	}
	// The one push that prunes. Unbatched, it is the whole push.
	res, _ := d.pushOnce(ctx, home, t, lim, stats, &counter, mirrorRefspecs, true)
	return res
}

// mirrorMayExceedBatch reports whether the mirror holds more refs than one batch, which is the only
// case in which a push could need batching. A mirror that fits is pushed as it always was, without
// the extra request to the replica that planning costs. When the refs cannot be counted it answers
// yes, and the plan decides.
func (d syncDeps) mirrorMayExceedBatch(ctx context.Context, home string, t syncTask, batch int) bool {
	var n lineCounter
	argv := []string{"git", "--git-dir=" + d.mirrors.Dir(t.RepoID), "for-each-ref", "--format=x", "refs/heads", "refs/tags"}
	if _, err := d.runBoundedOut(ctx, d.fetchTimeoutOrDefault(), "ref count", argv, gitBaseEnv(home), home, &n); err != nil {
		return true
	}
	return n.lines > batch
}

// lineCounter counts the lines written to it.
type lineCounter struct{ lines int }

func (c *lineCounter) Write(b []byte) (int, error) {
	c.lines += bytes.Count(b, []byte{'\n'})
	return len(b), nil
}

// pushOnce runs one push process and reports whether it succeeded; when it did not, the result says
// why. Each push gets its own timeout, and the token's expiry is judged immediately before it starts:
// a token the platform has declared expired is not used, whether or not the replica would still
// accept it. (A token that expires while a push is running is the replica's to judge; the connector's
// clock, which may be skewed, does not cut it short.) A cancelled context is not a failure and
// reports pending.
func (d syncDeps) pushOnce(ctx context.Context, home string, t syncTask, lim Limits, stats *syncStats, counter *pushCounter, refspecs []string, prune bool) (syncResult, bool) {
	if ctx.Err() != nil {
		return syncResult{State: StatePending}, false
	}
	if d.tokenExpired(t) {
		return syncResult{State: StateError, ErrorType: ErrPushTokenExpired}, false
	}
	// --porcelain is always used, so there is one push path whether or not statistics are collected.
	// It moves git's per-ref report (including each rejection's reason) from stderr to stdout, which
	// pushCounter reads and hands back to the classifier in the shape it expects.
	argv := []string{"git", "--git-dir=" + d.mirrors.Dir(t.RepoID), "push", "--porcelain"}
	if prune {
		argv = append(argv, "--prune")
	}
	argv = append(argv, "--force", "--", t.ReplicaURL)
	argv = append(argv, refspecs...)

	stderr, err := d.runBoundedOut(ctx, lim.pushTimeout(), "push", argv, d.replicaEnv(home, t, lim), home, counter)
	if err == nil && counter.Rejected() > 0 {
		// A rejected ref means the replica is not current, whatever the exit status says.
		err = errPushRejected
	}
	if err != nil {
		return d.pushFailure(ctx, home, t, stats, counter, err, stderr), false
	}
	return syncResult{State: StateOK}, true
}

// pushFailure classifies a failed push (or planning push).
func (d syncDeps) pushFailure(ctx context.Context, home string, t syncTask, stats *syncStats, counter *pushCounter, err error, stderr []byte) syncResult {
	if ctx.Err() != nil {
		return syncResult{State: StatePending}
	}
	stderr = append(append([]byte(nil), stderr...), counter.RejectionText()...)
	// Re-evaluated now: a long fetch can outlive the token, and a 401 then means "expired".
	typ, detail := classifyGitFailure(replicaSide, err, stderr, d.tokenExpired(t))
	res := syncResult{State: StateError, ErrorType: typ, ErrorDetail: detail}
	d.learnDefaultBranch(ctx, home, t, stats)
	return res
}

// planPush asks git what pushing the whole mirror would change, without sending anything. It is a
// request to the replica like any push, so it is bounded and authenticated like one.
func (d syncDeps) planPush(ctx context.Context, home string, t syncTask, lim Limits, stats *syncStats) (*pushPlan, syncResult, bool) {
	if ctx.Err() != nil {
		return nil, syncResult{State: StatePending}, false
	}
	if d.tokenExpired(t) {
		return nil, syncResult{State: StateError, ErrorType: ErrPushTokenExpired}, false
	}
	argv := []string{"git", "--git-dir=" + d.mirrors.Dir(t.RepoID), "push", "--dry-run", "--porcelain", "--prune", "--force", "--", t.ReplicaURL}
	argv = append(argv, mirrorRefspecs...)

	plan := &pushPlan{}
	var counter pushCounter
	stderr, err := d.runBoundedOut(ctx, lim.pushTimeout(), "push plan", argv, d.replicaEnv(home, t, lim), home, io.MultiWriter(plan, &counter))
	if err == nil && counter.Rejected() > 0 {
		err = errPushRejected
	}
	if err != nil {
		return nil, d.pushFailure(ctx, home, t, stats, &counter, err, stderr), false
	}
	if plan.unusable() {
		return nil, syncResult{State: StateError, ErrorType: ErrUnclassified, ErrorDetail: errPlanDetail}, false
	}
	sort.Strings(plan.upserts)
	sort.Strings(plan.deletes)
	return plan, syncResult{}, true
}

// pushPlan reads `git push --dry-run --porcelain` output: one "<flag>\t<from>:<to>\t<summary>" line
// per ref, where the flag is '*' (new), ' ' (fast-forward) or '+' (forced) for a ref the push would
// write, '-' for one it would delete, '=' for one already current and '!' for one it would refuse.
// Only the first two kinds are retained, by destination name; the rest is ignored (a rejection is
// pushCounter's business). The names come from the replica's advertisement and the mirror, so they are
// untrusted text: each must pass validPushRef or the whole plan is unusable.
type pushPlan struct {
	upserts, deletes []string
	// shadows are the names, on either side, of the form refs/heads/refs/... or refs/tags/refs/...
	// Only those can make a shorter name ambiguous (see splitConflicts), so only those are kept of
	// the up-to-date refs, which are most of a plan.
	shadows map[string]struct{}

	line     []byte
	overLong bool
	retained int
	bad      bool
}

func (p *pushPlan) Write(b []byte) (int, error) {
	for _, c := range b {
		if c == '\n' {
			p.endLine()
			continue
		}
		if len(p.line) < maxPlanLine {
			p.line = append(p.line, c)
		} else {
			p.overLong = true
		}
	}
	return len(b), nil
}

func (p *pushPlan) endLine() {
	line, over := p.line, p.overLong
	p.line, p.overLong = p.line[:0], false
	if len(line) < 2 || line[1] != '\t' {
		return // a header ("To <url>"), "Done", or blank
	}
	flag := line[0]
	switch flag {
	case '*', ' ', '+', '-', '=':
	default:
		return
	}
	if over {
		p.bad = true
		return
	}
	// The second field is "<from>:<to>", ended by a tab (or the end of the line).
	field := string(line[2:])
	if i := strings.IndexByte(field, '\t'); i >= 0 {
		field = field[:i]
	}
	from, to, ok := strings.Cut(field, ":")
	if !ok || !validPushRef(to) {
		p.bad = true
		return
	}
	if flag == '-' {
		if from != "" {
			p.bad = true
			return
		}
	} else if from != to {
		// The mirror's refspecs map every ref to itself.
		p.bad = true
		return
	}
	if p.retained += len(to); p.retained > maxPlanBytes {
		p.bad = true
		return
	}
	if isShadow(to) {
		if p.shadows == nil {
			p.shadows = map[string]struct{}{}
		}
		p.shadows[to] = struct{}{}
	}
	switch flag {
	case '-':
		p.deletes = append(p.deletes, to)
	case '=':
	default:
		p.upserts = append(p.upserts, to)
	}
}

// isShadow reports whether name is a branch or tag whose own name starts with "refs/", such as
// refs/heads/refs/heads/x.
func isShadow(name string) bool {
	rest, ok := strings.CutPrefix(name, "refs/heads/")
	if !ok {
		rest, ok = strings.CutPrefix(name, "refs/tags/")
	}
	return ok && strings.HasPrefix(rest, "refs/")
}

// unusable reports whether the plan must not be acted on: some status line was malformed, named an
// invalid ref, or the plan outgrew its bound.
func (p *pushPlan) unusable() bool {
	p.endLine() // a final line with no newline
	return p.bad
}

// validPushRef reports whether name may be placed in a refspec: a branch or tag, of bounded length,
// without a character git forbids in a ref name or that has meaning in a refspec.
func validPushRef(name string) bool {
	rest, ok := strings.CutPrefix(name, "refs/heads/")
	if !ok {
		rest, ok = strings.CutPrefix(name, "refs/tags/")
	}
	if !ok || rest == "" || len(name) > maxPlanRefLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c < 0x20, c == 0x7f:
			return false
		case strings.IndexByte(" :*?[\\^~", c) >= 0:
			return false
		}
	}
	return true
}

// splitConflicts separates the refs a push would write into those that can go in an explicit batch
// and those that cannot, which are left to the final push (whose glob refspecs name no ref):
//   - a ref that is a directory prefix of a ref the push would delete, or has one as its own prefix
//     (the replica still holds refs/heads/x/y while the mirror now has refs/heads/x). An explicit push
//     of such a ref fails to lock it; the final push deletes the other and writes this one together;
//   - a ref whose name another ref extends in a way git's name lookup also accepts: with both
//     refs/heads/x and refs/heads/refs/heads/x, the refspec refs/heads/x matches more than one ref and
//     git refuses it (shadows holds every such name of the plan, on either side);
//   - a ref too long to put in an argument list with others.
func splitConflicts(upserts, deletes []string, shadows map[string]struct{}) (free, held []string) {
	deleted := make(map[string]struct{}, len(deletes))
	dirs := map[string]struct{}{}
	for _, d := range deletes {
		deleted[d] = struct{}{}
		for i := 0; i < len(d); i++ {
			if d[i] == '/' {
				dirs[d[:i]] = struct{}{}
			}
		}
	}
	for _, u := range upserts {
		conflict := len(u) > maxBatchRefLen
		if _, ok := dirs[u]; ok {
			conflict = true
		}
		for _, prefix := range []string{"refs/heads/", "refs/tags/"} {
			if _, ok := shadows[prefix+u]; ok {
				conflict = true
			}
		}
		for i := 0; i < len(u) && !conflict; i++ {
			if u[i] == '/' {
				_, conflict = deleted[u[:i]]
			}
		}
		if conflict {
			held = append(held, u)
		} else {
			free = append(free, u)
		}
	}
	return free, held
}

// partitionBatches cuts refs, in order, into batches of at most maxRefs refs and at most maxBytes of
// refspec text. A ref whose own refspec exceeds maxBytes still gets a batch to itself.
func partitionBatches(refs []string, maxRefs, maxBytes int) [][]string {
	var out [][]string
	var cur []string
	size := 0
	for _, ref := range refs {
		cost := 2*len(ref) + 3 // "+ref:ref" and its argv separator
		if len(cur) > 0 && (len(cur) >= maxRefs || size+cost > maxBytes) {
			out = append(out, slices.Clone(cur))
			cur, size = cur[:0], 0
		}
		cur = append(cur, ref)
		size += cost
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}
