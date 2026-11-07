//go:build rig

package gitsync

import (
	"bufio"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file is the origin side of the slow rig (see replicarig_slow_test.go): it builds synthetic origin
// repositories with `git fast-import`, so nothing is checked in and a repository of hundreds of MiB
// takes seconds, not minutes, to make.

// rigKnobs are the sizes a run can be tuned with, each from an environment variable. The defaults are
// what the documented command runs.
type rigKnobs struct {
	LargeMiB      int // RIG_LARGE_MIB: size of the large-repository scenario's origin
	Refs          int // RIG_REFS: branches plus tags in the many-refs scenarios
	BatchRefs     int // RIG_BATCH_REFS: push_batch_refs of the batched runs (the platform accepts 100 to 100000)
	DeletePercent int // RIG_DELETE_PERCENT: share of the refs the origin drops before the deletion scenarios
	SlowPushSecs  int // RIG_SLOW_PUSH_SECONDS: how long the throttled replica takes over one push
}

func rigKnobsFromEnv(t *testing.T) rigKnobs {
	t.Helper()
	return rigKnobs{
		LargeMiB:      rigEnvInt(t, "RIG_LARGE_MIB", 512, 1, 65536),
		Refs:          rigEnvInt(t, "RIG_REFS", 10000, 200, 200000),
		BatchRefs:     rigEnvInt(t, "RIG_BATCH_REFS", 1000, 100, 100000),
		DeletePercent: rigEnvInt(t, "RIG_DELETE_PERCENT", 90, 1, 99),
		SlowPushSecs:  rigEnvInt(t, "RIG_SLOW_PUSH_SECONDS", 90, 70, 3600),
	}
}

func rigEnvInt(t *testing.T, name string, def, lo, hi int) int {
	t.Helper()
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		t.Fatalf("%s=%q: want an integer from %d to %d", name, v, lo, hi)
	}
	return n
}

// fastImport runs `git fast-import` into the bare repository at gitDir and hands write the stream's
// writer. Objects are written uncompressed (pack.compression=0): the generated blobs are random, so
// compression would only cost time.
func fastImport(t *testing.T, gitDir string, write func(w *bufio.Writer) error) {
	t.Helper()
	cmd := exec.Command("git", "-c", "pack.compression=0", "--git-dir="+gitDir, "fast-import", "--quiet", "--done")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("git fast-import: %v", err)
	}
	w := bufio.NewWriterSize(in, 1<<20)
	werr := write(w)
	if werr == nil {
		_, werr = io.WriteString(w, "done\n")
	}
	if werr == nil {
		werr = w.Flush()
	}
	_ = in.Close()
	if err := cmd.Wait(); err != nil || werr != nil {
		t.Fatalf("git fast-import: %v (writing the stream: %v)\n%s", err, werr, stderr.String())
	}
}

// rigCommit writes one commit on branch with the given blobs, each a path and a size in bytes of
// pseudo-random content; mark is the commit's mark.
func rigCommit(w *bufio.Writer, rng *rand.ChaCha8, branch string, mark int, parent int, msg string, files map[string]int) error {
	when := 1893456000 + mark // 2030-01-01, one second apart: stable and in no way "now"
	fmt.Fprintf(w, "commit %s\nmark :%d\ncommitter Rig <rig@example.test> %d +0000\ndata %d\n%s\n", branch, mark, when, len(msg), msg)
	if parent > 0 {
		fmt.Fprintf(w, "from :%d\n", parent)
	}
	for path, size := range files {
		fmt.Fprintf(w, "M 100644 inline %s\ndata %d\n", path, size)
		if err := copyRandom(w, rng, size); err != nil {
			return err
		}
		w.WriteByte('\n')
	}
	return nil
}

// copyRandom writes n bytes of the generator's output.
func copyRandom(w io.Writer, rng *rand.ChaCha8, n int) error {
	buf := make([]byte, 1<<20)
	for n > 0 {
		chunk := buf[:min(n, len(buf))]
		_, _ = rng.Read(chunk) // ChaCha8.Read never fails
		if _, err := w.Write(chunk); err != nil {
			return err
		}
		n -= len(chunk)
	}
	return nil
}

// genLargeOrigin creates a bare repository at gitDir holding about totalMiB MiB of incompressible
// blobs spread over commits commits on main, plus a few branches and tags. It returns the number of
// bytes of blob content written.
func genLargeOrigin(t *testing.T, gitDir string, totalMiB, commits int) int64 {
	t.Helper()
	git(t, "", "init", "--bare", "-b", "main", gitDir)
	rng := rand.NewChaCha8([32]byte{'r', 'i', 'g', '-', 'l', 'a', 'r', 'g', 'e'})
	per := totalMiB * mebibyte / commits
	var written int64
	fastImport(t, gitDir, func(w *bufio.Writer) error {
		for i := 1; i <= commits; i++ {
			parent := i - 1
			files := map[string]int{fmt.Sprintf("data/blob-%04d.bin", i): per}
			if err := rigCommit(w, rng, "refs/heads/main", i, parent, fmt.Sprintf("commit %d", i), files); err != nil {
				return err
			}
			written += int64(per)
		}
		// A handful of other tips, so the ref comparison is not a single branch.
		for i, mark := range []int{commits / 4, commits / 2, commits - 1} {
			fmt.Fprintf(w, "reset refs/heads/release/%d\nfrom :%d\n\n", i, mark)
			fmt.Fprintf(w, "reset refs/tags/v1.%d\nfrom :%d\n\n", i, mark)
		}
		return nil
	})
	return written
}

// rigRefName is the name of the i-th generated ref: half branches, half tags, in realistic
// directories. They never collide and none is a prefix of another.
func rigRefName(i int) string {
	if i%2 == 0 {
		return fmt.Sprintf("refs/heads/team-%02d/topic-%06d", i%37, i)
	}
	return fmt.Sprintf("refs/tags/v%d.%d.%d", 1+i%9, (i/9)%100, i)
}

// genManyRefsOrigin creates a bare repository at gitDir with main (a short history of small commits)
// and refs further branches and tags, each pointing at one of those commits. It returns the generated
// ref names.
func genManyRefsOrigin(t *testing.T, gitDir string, refs int) []string {
	t.Helper()
	git(t, "", "init", "--bare", "-b", "main", gitDir)
	rng := rand.NewChaCha8([32]byte{'r', 'i', 'g', '-', 'r', 'e', 'f', 's'})
	const commits = 64
	names := make([]string, refs)
	fastImport(t, gitDir, func(w *bufio.Writer) error {
		for i := 1; i <= commits; i++ {
			files := map[string]int{fmt.Sprintf("f-%02d.txt", i): 512}
			if err := rigCommit(w, rng, "refs/heads/main", i, i-1, fmt.Sprintf("commit %d", i), files); err != nil {
				return err
			}
		}
		for i := range names {
			names[i] = rigRefName(i)
			fmt.Fprintf(w, "reset %s\nfrom :%d\n\n", names[i], 1+i%commits)
		}
		return nil
	})
	// One packed-refs file, as a forge keeps for a repository with this many refs.
	git(t, "", "--git-dir="+gitDir, "pack-refs", "--all")
	return names
}

// refMap returns every branch and tag of the repository as name to object id.
func refMap(t *testing.T, gitDir string) map[string]string {
	t.Helper()
	out := git(t, "", "--git-dir="+gitDir, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags")
	m := map[string]string{}
	if out == "" {
		return m
	}
	for _, line := range strings.Split(out, "\n") {
		sha, name, _ := strings.Cut(line, " ")
		m[name] = sha
	}
	return m
}

// diffRefMaps describes how got differs from want, at most a few entries of each kind; "" when equal.
func diffRefMaps(want, got map[string]string) string {
	var missing, extra, differ []string
	for n, sha := range want {
		switch g, ok := got[n]; {
		case !ok:
			missing = append(missing, n)
		case g != sha:
			differ = append(differ, n)
		}
	}
	for n := range got {
		if _, ok := want[n]; !ok {
			extra = append(extra, n)
		}
	}
	if len(missing)+len(extra)+len(differ) == 0 {
		return ""
	}
	show := func(s []string) string { return fmt.Sprintf("%d (e.g. %v)", len(s), s[:min(3, len(s))]) }
	return fmt.Sprintf("missing %s, extra %s, different %s", show(missing), show(extra), show(differ))
}

// dirSize sums the apparent size of the regular files under dir, the way mirrorExceeds does.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // a file that vanished mid-walk (git repacking) is not an error here
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// peakSampler records the largest size dir reaches while it runs.
type peakSampler struct {
	dir  string
	stop chan struct{}
	done chan struct{}
	peak int64
	once sync.Once
}

func startPeakSampler(dir string, every time.Duration) *peakSampler {
	s := &peakSampler{dir: dir, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			s.peak = max(s.peak, dirSize(dir))
			select {
			case <-s.stop:
				return
			case <-tick.C:
			}
		}
	}()
	return s
}

// Stop ends sampling and returns the peak, including one final sample.
func (s *peakSampler) Stop() int64 {
	s.once.Do(func() {
		close(s.stop)
		<-s.done
		s.peak = max(s.peak, dirSize(s.dir))
	})
	return s.peak
}

func mib(b int64) float64 { return float64(b) / mebibyte }
