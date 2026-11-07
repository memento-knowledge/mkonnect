//go:build rig

package gitsync

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// rigRSS observes the peak resident memory of the git processes the connector starts, without
// touching the connector: for the length of one sync it puts a `git` wrapper first on PATH that runs
// the real git under /usr/bin/time, which reports the largest resident set of the process and the
// children it waited for (git-remote-https, pack-objects, index-pack). The wrapper acts only when HOME
// is one of the connector's own temp homes, so the origin and replica servers' git processes and the
// rig's own git calls are not measured. The measurement goes to a file (-o), never to git's stderr.
//
// The wrapper is written into a temporary directory at run time; nothing of it is checked in. The
// number is a best effort: where /usr/bin/time does not support the flags below, there is no number
// and the rig says so.
type rigRSS struct {
	dir  string
	gnu  bool // /usr/bin/time reports kibibytes (GNU); otherwise bytes (BSD, macOS)
	prev string
}

// newRigRSS returns nil, and logs why, when the measurement is not possible here.
func newRigRSS(t *testing.T) *rigRSS {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		return nil
	}
	timeBin := "/usr/bin/time"
	probe := filepath.Join(t.TempDir(), "probe")
	r := &rigRSS{dir: t.TempDir()}
	switch {
	case exec.Command(timeBin, "-f", "%M", "-o", probe, "true").Run() == nil:
		r.gnu = true
	case exec.Command(timeBin, "-l", "-o", probe, "true").Run() == nil:
	default:
		t.Logf("RIG peak RSS: skipped (%s does not support -f %%M or -l with -o)", timeBin)
		return nil
	}
	flags := "-l"
	if r.gnu {
		flags = "-f %M"
	}
	script := fmt.Sprintf(`#!/bin/sh
case "${HOME##*/}" in
%s*) exec %s %s -o '%s'/rss.$$ '%s' "$@" ;;
esac
exec '%s' "$@"
`, tempHomePrefix, timeBin, flags, r.dir, realGit, realGit)
	if err := os.WriteFile(filepath.Join(r.dir, "git"), []byte(script), 0o700); err != nil {
		t.Logf("RIG peak RSS: skipped (%v)", err)
		return nil
	}
	return r
}

// begin puts the wrapper first on PATH and returns a function that restores PATH and reports the
// largest resident set seen, in bytes (0 when none was recorded).
func (r *rigRSS) begin() func() int64 {
	r.prev = os.Getenv("PATH")
	os.Setenv("PATH", r.dir+string(os.PathListSeparator)+r.prev)
	var (
		once sync.Once
		peak int64
	)
	return func() int64 {
		once.Do(func() {
			os.Setenv("PATH", r.prev)
			files, _ := filepath.Glob(filepath.Join(r.dir, "rss.*"))
			for _, f := range files {
				b, err := os.ReadFile(f)
				_ = os.Remove(f)
				if err != nil {
					continue
				}
				if v := r.parse(string(b)); v > peak {
					peak = v
				}
			}
		})
		return peak
	}
}

// parse reads one measurement file: GNU time wrote "<KiB>", BSD time a report with a line
// "<bytes>  maximum resident set size".
func (r *rigRSS) parse(s string) int64 {
	if r.gnu {
		kib, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return 0
		}
		return kib << 10
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "maximum resident set size") {
			if f := strings.Fields(line); len(f) > 0 {
				if v, err := strconv.ParseInt(f[0], 10, 64); err == nil {
					return v
				}
			}
		}
	}
	return 0
}
