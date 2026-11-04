package gitsync

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// This file decides whether a synced repository uses Git LFS, so the status can carry the
// lfs_not_replicated warning. The connector mirrors and pushes branches and tags only: an LFS pointer
// file is replicated like any other blob, but the large object it names is not, so a replica of such a
// repository holds pointers that cannot be resolved.
//
// The check is a heuristic, deliberately cheap and local. It reads the repository root's
// .gitattributes at the tip of the origin's default branch from the mirror (two git plumbing
// invocations, no checkout, no network, no credential, nothing that runs a hook) and looks for an
// attribute `filter=lfs`, which is what `git lfs track` writes. Its limits are part of its contract:
//
//   - Only the default branch's tip is examined. A repository whose LFS rules live only on another
//     branch, or only in older history, is not flagged (a false negative).
//   - Only the root .gitattributes is read: nested .gitattributes files, attributes kept outside the
//     repository (.git/info/attributes, a user's global file) and pointer files committed under some
//     other rule are not seen. A rule with no pointer files behind it, or one defined in a macro that
//     nothing uses, still flags the repository (a false positive).
//   - The ref is looked up by its full name, but git still falls back to other namespaces (a tag
//     literally named refs/heads/<branch>) when the branch is missing from the mirror. That can only
//     misreport a repository whose origin chose to confuse it, and it bounds nothing.
//   - It distinguishes "no" from "cannot tell" (the default branch was not learned in this sync, git
//     failed or timed out, or the file is larger than it will read). Only "no" clears a warning the
//     repository already carried: finishSync keeps the previous warnings across an undecided sync, so
//     a failed lookup does not make the warning flap off for a whole sync interval.
//   - The warning is not persisted in the run state. It is a function of the current mirror,
//     recomputed by every sync, and the status after a restart reports no result at all (pending)
//     until the first sync, so persisting it would add a record field, its validation and its
//     invalidation on a target change for nothing the platform could see.
//
// The work is bounded however hostile the repository: the file is only read when its size, known
// beforehand, is within maxAttributesBytes; it is streamed through a constant-memory scanner; and the
// whole lookup runs under lfsDetectTimeout. A failure never fails the sync and is logged by a fixed
// reason word only, never with git's output, a ref name or a path, all of which come from the origin.

const (
	// lfsDetectTimeout bounds the whole lookup. Both git invocations read a handful of local objects.
	lfsDetectTimeout = 30 * time.Second
	// maxAttributesBytes is the largest .gitattributes read. Real ones are a few kilobytes; git itself
	// ignores one over 100 MiB, which would only waste time here.
	maxAttributesBytes = 1 << 20
	// maxAttributesLine is git's own limit: a line of this many bytes or more is ignored.
	maxAttributesLine = 2048
	// maxLsTreeOutput bounds the ls-tree output retained; one entry for one path is far smaller, so
	// output that exceeds it is not an entry.
	maxLsTreeOutput = 1 << 10

	attributesFile = ".gitattributes"
	// lfsFilterAttribute is the attribute assignment `git lfs track` writes, as a whole field.
	lfsFilterAttribute = "filter=lfs"
	utf8BOM            = "\xef\xbb\xbf"
)

// Reasons detection did not reach a verdict, logged as-is. They are fixed words: nothing from git or
// the origin is logged.
const (
	lfsReasonNoBranch  = "default_branch_unknown"
	lfsReasonGit       = "git_failed"
	lfsReasonTimeout   = "timeout"
	lfsReasonMalformed = "unexpected_output"
	lfsReasonTooLarge  = "attributes_too_large"
)

// detectLFS returns the warnings for a repository that was just synced successfully: the LFS warning
// when its default branch's .gitattributes assigns the LFS filter, otherwise nil. decided is false
// when no verdict was reached, in which case the caller keeps whatever it reported before. branch is
// the origin's default branch as learned in this sync (already vetted by validBranchName), or empty
// when it could not be. Like the other statistics it needs the standard-output seam. Nothing here can
// fail the sync.
func (d syncDeps) detectLFS(ctx context.Context, home string, t syncTask, branch string) (warnings []string, decided bool) {
	if d.runOut == nil || ctx.Err() != nil {
		return nil, false
	}
	found, reason := d.usesLFS(ctx, home, t, branch)
	switch {
	case reason != "":
		slog.Debug("lfs detection undecided", "repo_id", t.RepoID, "reason", reason)
		return nil, false
	case found:
		return []string{WarnLFSNotReplicated}, true
	}
	return nil, true
}

// usesLFS reports whether the default branch's root .gitattributes assigns the LFS filter. A non-empty
// reason means no verdict was reached and found is meaningless.
func (d syncDeps) usesLFS(ctx context.Context, home string, t syncTask, branch string) (found bool, reason string) {
	if branch == "" {
		return false, lfsReasonNoBranch
	}
	ctx, cancel := context.WithTimeout(ctx, lfsDetectTimeout)
	defer cancel()

	// Read what the branch really holds, whatever a replace ref in a mirror made by an older version
	// says; the branch name is fully qualified so it cannot be read as an option or another ref.
	env := append(gitBaseEnv(home), "GIT_NO_REPLACE_OBJECTS=1")
	gitDir := "--git-dir=" + d.mirrors.Dir(t.RepoID)

	listing := &cappedBuffer{max: maxLsTreeOutput}
	lsTree := []string{"git", gitDir, "ls-tree", "-l", "-z", "refs/heads/" + branch, "--", attributesFile}
	if _, err := d.runBoundedOut(ctx, lfsDetectTimeout, "lfs lookup", lsTree, env, home, listing); err != nil {
		return false, lfsFailure(err)
	}
	if listing.over {
		return false, lfsReasonMalformed
	}
	entry, err := parseAttributesEntry(listing.buf)
	switch {
	case errors.Is(err, errAttributesAbsent):
		return false, ""
	case errors.Is(err, errAttributesTooLarge):
		return false, lfsReasonTooLarge
	case err != nil:
		return false, lfsReasonMalformed
	}

	var scan attributesScanner
	catFile := []string{"git", gitDir, "cat-file", "blob", entry}
	if _, err := d.runBoundedOut(ctx, lfsDetectTimeout, "lfs read", catFile, env, home, &scan); err != nil {
		return false, lfsFailure(err)
	}
	return scan.Found(), ""
}

func lfsFailure(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return lfsReasonTimeout
	}
	return lfsReasonGit
}

// cappedBuffer keeps the first max bytes written to it and notes that more came, so a strict parse
// never sees output that was cut in the middle. Write consumes everything, so git never sees an error.
type cappedBuffer struct {
	max  int
	buf  []byte
	over bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.max - len(b.buf); len(p) > room {
		b.over = true
		p = p[:max(room, 0)]
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

var (
	// errAttributesAbsent: the branch has no .gitattributes git would read (none, or a directory,
	// symlink or submodule of that name, which git ignores).
	errAttributesAbsent    = errors.New("no readable .gitattributes")
	errAttributesTooLarge  = errors.New(".gitattributes exceeds the size read")
	errAttributesMalformed = errors.New("unexpected ls-tree output")
)

// parseAttributesEntry reads the output of `git ls-tree -l -z <ref> -- .gitattributes`, one entry
// "<mode> <type> <object> <size>\t<path>\0" or nothing, and returns the object id of a regular file
// small enough to read.
func parseAttributesEntry(out []byte) (object string, err error) {
	if len(out) == 0 {
		return "", errAttributesAbsent
	}
	rest, ok := strings.CutSuffix(string(out), "\x00")
	if !ok {
		return "", errAttributesMalformed
	}
	meta, path, ok := strings.Cut(rest, "\t")
	fields := strings.Fields(meta)
	if !ok || path != attributesFile || len(fields) != 4 {
		return "", errAttributesMalformed
	}
	mode, typ, object, size := fields[0], fields[1], fields[2], fields[3]
	switch {
	case mode == "120000" || mode == "040000" || mode == "160000":
		// A symlink, directory or submodule: git does not read it as an attributes file.
		return "", errAttributesAbsent
	case (mode != "100644" && mode != "100755") || typ != "blob" || !validObjectID(object):
		return "", errAttributesMalformed
	}
	n, perr := strconv.ParseUint(size, 10, 63)
	if perr != nil {
		return "", errAttributesMalformed
	}
	if n > maxAttributesBytes {
		return "", errAttributesTooLarge
	}
	return object, nil
}

// validObjectID reports whether s is a full lowercase hex object name, SHA-1 or SHA-256. It is
// handed to git as an argument, so nothing else may pass.
func validObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// attributesScanner reads a .gitattributes file as a stream, in constant memory, and notes whether any
// line assigns the LFS filter. It parses as git does for what matters here: leading whitespace is
// skipped, a blank line or one starting with '#' is ignored, a line of maxAttributesLine bytes or more
// is ignored, a UTF-8 byte order mark on the first line is skipped, and the attributes are the
// whitespace-separated fields after the pattern. It inspects at most maxAttributesBytes; Write always
// consumes everything it is given, so git never sees a short write.
type attributesScanner struct {
	line  []byte
	long  bool // the current line reached maxAttributesLine: ignore the rest of it
	lines int
	total int
	found bool
}

func (s *attributesScanner) Write(p []byte) (int, error) {
	n := len(p)
	if s.found || s.total >= maxAttributesBytes {
		s.total = min(s.total+n, maxAttributesBytes)
		return n, nil
	}
	if room := maxAttributesBytes - s.total; len(p) > room {
		p = p[:room]
	}
	s.total += len(p)
	for _, c := range p {
		if c == '\n' {
			s.endLine()
			if s.found {
				break
			}
			continue
		}
		if s.long {
			continue
		}
		if len(s.line) >= maxAttributesLine-1 {
			s.long = true
			s.line = s.line[:0]
			continue
		}
		s.line = append(s.line, c)
	}
	return n, nil
}

func (s *attributesScanner) endLine() {
	if !s.long && assignsLFSFilter(s.line, s.lines == 0) {
		s.found = true
	}
	s.line, s.long = s.line[:0], false
	s.lines++
}

// Found reports whether a line assigned the LFS filter, settling a last line with no newline.
func (s *attributesScanner) Found() bool {
	if !s.found && (len(s.line) > 0 || s.long) {
		s.endLine()
	}
	return s.found
}

// assignsLFSFilter reports whether one .gitattributes line, without its newline, sets filter=lfs.
func assignsLFSFilter(line []byte, first bool) bool {
	text := string(line)
	if first {
		text = strings.TrimPrefix(text, utf8BOM)
	}
	// git splits on ASCII whitespace only, and ignores a comment and a negative pattern.
	fields := strings.FieldsFunc(text, func(r rune) bool { return strings.ContainsRune(" \t\r\v\f", r) })
	if len(fields) == 0 || fields[0][0] == '#' || fields[0][0] == '!' {
		return false
	}
	for _, attr := range fields[1:] {
		if attr == lfsFilterAttribute {
			return true
		}
	}
	return false
}
