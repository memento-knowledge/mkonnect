package gitsync

import (
	"context"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// This file holds what a sync learns from git's standard output: how many refs a push changed, the
// newest commit at the origin, and the origin's default branch. git's output is consumed as a stream
// by writers that keep only counters, so its size never matters: a push of a hundred thousand refs is
// counted exactly in constant memory, where a size-capped buffer would silently undercount it.

// outRunFunc is runFunc with git's standard output streamed to stdout instead of discarded. It is a
// separate seam, rather than a change to runFunc, because most fakes only care about stderr.
type outRunFunc func(ctx context.Context, argv, env []string, dir string, stdout io.Writer) ([]byte, error)

const (
	// maxRejectedLine bounds how much of one rejected-ref line is retained.
	maxRejectedLine = 1 << 10
	// maxRejectionText bounds the rejection text retained from one push, which is appended to git's
	// stderr for classification.
	maxRejectionText = 8 << 10
	// maxFormatLine bounds a line of the for-each-ref format below, which is two integers.
	maxFormatLine = 64
	// maxCommitUnix is 9999-12-31T23:59:59Z, the last instant RFC3339 can express. A committer date
	// comes from the origin and is arbitrary, so anything beyond it is dropped rather than reported.
	maxCommitUnix = 253402300799
	// maxBranchNameLength bounds the origin default branch echoed in a status.
	maxBranchNameLength = 255
	// maxSymrefOutput bounds the `ls-remote --symref` output that is retained.
	maxSymrefOutput = 64 << 10
)

// pushCounter reads `git push --porcelain` output. A status line is "<flag>\t<from>:<to>\t<summary>",
// where the flag is '*' for a new ref, ' ' for a fast-forward, '+' for a forced update, '-' for a
// deletion, '=' for a ref already up to date and '!' for a rejected one. The first four are the refs
// that changed on the replica and are counted; '=' is not. Only the first two bytes of a line are
// inspected (the rest of a line is never buffered), except for a rejected line, whose summary is
// retained briefly: --porcelain moves git's per-ref rejection explanation from stderr to stdout, and
// the failure classifier reads stderr, so RejectionText gives it back in the shape it expects. The ref
// pair before the summary is skipped however long it is, so a long ref name cannot crowd the
// explanation out.
//
// The counters accumulate across pushes, so a push made in batches can share one counter.
type pushCounter struct {
	refs, rejected int
	text           []byte

	col     int
	flag    byte
	tab     bool   // the byte after the flag was a tab: this is a status line
	tabs    int    // tabs seen on the line so far, counting the one after the flag
	summary []byte // of a rejected line, bounded
}

func (p *pushCounter) Write(b []byte) (int, error) {
	for _, c := range b {
		if c == '\n' {
			p.endLine()
			continue
		}
		switch p.col {
		case 0:
			p.flag = c
		case 1:
			if p.tab = c == '\t'; p.tab {
				p.tabs = 1
			}
		default:
			if p.flag == '!' && p.tab {
				switch {
				case p.tabs >= 2:
					if len(p.summary) < maxRejectedLine {
						p.summary = append(p.summary, c)
					}
				case c == '\t':
					p.tabs = 2
				}
			}
		}
		p.col++
	}
	return len(b), nil
}

// endLine accounts for the line just completed and resets for the next.
func (p *pushCounter) endLine() {
	if p.col >= 2 && p.tab {
		switch p.flag {
		case '*', ' ', '+', '-':
			p.refs++
		case '!':
			p.rejected++
			p.retainRejection()
		}
	}
	p.col, p.flag, p.tab, p.tabs, p.summary = 0, 0, false, 0, p.summary[:0]
}

// retainRejection appends the rejected line, rewritten as git prints it without --porcelain, to the
// retained text. The ref names are replaced by fixed placeholders: the classifier discards them
// anyway, and a shape that lacked them would let a branch called "no-space-left-on-device" steer it.
func (p *pushCounter) retainRejection() {
	row := " ! ref -> ref"
	if summary := strings.TrimSuffix(string(p.summary), "\r"); summary != "" {
		row += " " + summary
	}
	row += "\n"
	if len(p.text)+len(row) <= maxRejectionText {
		p.text = append(p.text, row...)
	}
}

// settle completes a final line that had no newline.
func (p *pushCounter) settle() {
	if p.col > 0 {
		p.endLine()
	}
}

// Refs is how many refs the push created, updated or deleted.
func (p *pushCounter) Refs() int { p.settle(); return p.refs }

// Rejected is how many refs the replica refused. A push that reports any has failed, whatever its
// exit status.
func (p *pushCounter) Rejected() int { p.settle(); return p.rejected }

// RejectionText is the retained rejection explanation, in git's non-porcelain stderr shape.
func (p *pushCounter) RejectionText() []byte { p.settle(); return p.text }

// newestFormat is the for-each-ref format newestTracker reads: the committer date of the ref's
// commit, then the committer date of the commit a tag object peels to. A branch or lightweight tag
// fills the first; an annotated tag fills only the second (git before 2.44 peels one level, so a tag
// of a tag may fill neither, which can only under-report); a tag of a tree or blob fills neither. A
// tag object's own date is never selected, because it says when the tag was made, not when the code
// last changed.
const newestFormat = "%(committerdate:unix)|%(*committerdate:unix)"

// newestTracker reads newestFormat lines and keeps the latest usable time. A committer date comes from
// the origin and can be anything, so times after ceiling (a Unix time; zero means the latest RFC3339
// can express) are not credible and are dropped.
type newestTracker struct {
	ceiling int64
	max     int64
	line    []byte
	over    bool
}

func (n *newestTracker) Write(b []byte) (int, error) {
	for _, c := range b {
		if c == '\n' {
			n.endLine()
			continue
		}
		if len(n.line) < maxFormatLine {
			n.line = append(n.line, c)
		} else {
			n.over = true
		}
	}
	return len(b), nil
}

func (n *newestTracker) endLine() {
	if !n.over {
		ceiling := n.ceiling
		if ceiling <= 0 || ceiling > maxCommitUnix {
			ceiling = maxCommitUnix
		}
		for f := range strings.SplitSeq(string(n.line), "|") {
			if v, err := strconv.ParseInt(f, 10, 64); err == nil && v > 0 && v <= ceiling && v > n.max {
				n.max = v
			}
		}
	}
	n.line, n.over = n.line[:0], false
}

// Newest returns the newest commit time seen, in UTC, and false when no usable line was seen.
func (n *newestTracker) Newest() (time.Time, bool) {
	if len(n.line) > 0 {
		n.endLine()
	}
	if n.max == 0 {
		return time.Time{}, false
	}
	return time.Unix(n.max, 0).UTC(), true
}

// parseSymrefHead extracts the default branch from `git ls-remote --symref <url> HEAD`, whose first
// line is "ref: refs/heads/<name>\tHEAD" when HEAD is a symbolic ref. It reports false for a detached
// HEAD, a symref outside refs/heads, or a name that is not safe to echo in a status.
func parseSymrefHead(out []byte) (string, bool) {
	for line := range strings.SplitSeq(string(out), "\n") {
		rest, ok := strings.CutPrefix(line, "ref: refs/heads/")
		if !ok {
			continue
		}
		name, ok := strings.CutSuffix(rest, "\tHEAD")
		if !ok {
			continue
		}
		if validBranchName(name) {
			return name, true
		}
		return "", false
	}
	return "", false
}

// validBranchName reports whether name is safe to echo in a status and could be a branch name: bounded
// valid UTF-8, without control or format characters (which can reorder or hide text) and without what
// git itself forbids in a ref name. The origin chooses what it advertises, so none of it is trusted.
func validBranchName(name string) bool {
	if name == "" || len(name) > maxBranchNameLength || !utf8.ValidString(name) {
		return false
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") ||
		strings.HasSuffix(name, ".lock") || strings.Contains(name, "..") || strings.Contains(name, "//") ||
		strings.Contains(name, "@{") {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || strings.ContainsRune(" ~^:?*[\\", r)
	})
}
