package gitsync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	lfsSHA1   = "5dcfa042fb9b025599cd26145fd324dce4651d5d"
	lfsSHA256 = "5dcfa042fb9b025599cd26145fd324dce4651d5d5dcfa042fb9b025599cd2614"
	lfsRule   = "*.psd filter=lfs diff=lfs merge=lfs -text\n"
)

// lsTreeEntry is one entry of `git ls-tree -l -z` output for .gitattributes: the size is right-aligned
// in a seven-character column, and the entry ends with a NUL.
func lsTreeEntry(mode, typ, object, size string) string {
	return fmt.Sprintf("%s %s %s %7s\t.gitattributes\x00", mode, typ, object, size)
}

func TestAttributesScanner(t *testing.T) {
	long := func(n int) string { return "*.bin filter=lfs" + strings.Repeat(" ", n-len("*.bin filter=lfs")) }
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"what git lfs track writes", lfsRule, true},
		{"without a trailing newline", "*.psd filter=lfs", true},
		{"among other lines", "* text=auto\n*.sh eol=lf\n" + lfsRule + "*.png binary\n", true},
		{"attribute order does not matter", "*.bin -text merge=lfs diff=lfs filter=lfs\n", true},
		{"leading whitespace", "  \t*.bin filter=lfs\n", true},
		{"crlf line ends", "*.bin text\r\n*.psd filter=lfs\r\n", true},
		{"byte order mark on the first line", "\xef\xbb\xbf*.psd filter=lfs\n", true},
		{"a macro definition", "[attr]lfs filter=lfs diff=lfs merge=lfs -text\n", true},
		{"quoted pattern containing a space", "\"my file.bin\" filter=lfs\n", true},
		{"a comment", "# *.psd filter=lfs\n", false},
		{"an indented comment", "  # *.psd filter=lfs\n", false},
		{"a different filter", "*.psd filter=other\n", false},
		{"a longer filter name", "*.psd filter=lfsx\n", false},
		{"a prefixed filter name", "*.psd filter=notlfs\n", false},
		{"the filter unset", "*.psd -filter\n", false},
		{"the filter unspecified", "*.psd !filter\n", false},
		{"the assignment as the pattern", "filter=lfs\n", false},
		{"a pattern alone", "*.psd\n", false},
		{"no attributes file content", "\n\n\n", false},
		{"a line of 2047 bytes is read", long(2047) + "\n", true},
		{"a line of 2048 bytes is ignored as git ignores it", long(2048) + "\n", false},
		{"an ignored line does not hide the next", long(5000) + "\n" + lfsRule, true},
		{"an ignored last line", lfsRule[:0] + long(3000), false},
		{"a negative pattern", "!*.psd filter=lfs\n", false},
		{"a non-ascii space does not separate fields", "*.psd\u00a0filter=lfs\n", false},
		{"a vertical tab separates fields", "*.psd\vfilter=lfs\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var whole attributesScanner
			if n, err := whole.Write([]byte(tt.in)); n != len(tt.in) || err != nil {
				t.Fatalf("Write = %d, %v, want everything consumed", n, err)
			}
			if got := whole.Found(); got != tt.want {
				t.Errorf("one write: Found = %v, want %v", got, tt.want)
			}
			// Where a read is cut must not matter.
			var bytewise attributesScanner
			for i := range len(tt.in) {
				if n, err := bytewise.Write([]byte{tt.in[i]}); n != 1 || err != nil {
					t.Fatalf("Write = %d, %v", n, err)
				}
			}
			if got := bytewise.Found(); got != tt.want {
				t.Errorf("byte at a time: Found = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAttributesScannerIsBounded(t *testing.T) {
	filler := strings.Repeat("#\n", 1024) // 2 KiB of comments

	t.Run("a rule within the bound is found", func(t *testing.T) {
		var s attributesScanner
		s.Write([]byte(strings.Repeat(filler, 400))) // 800 KiB
		s.Write([]byte(lfsRule))
		if !s.Found() {
			t.Error("a rule inside the first MiB was missed")
		}
	})
	t.Run("a rule beyond the bound is not read", func(t *testing.T) {
		var s attributesScanner
		for range maxAttributesBytes/len(filler) + 1 {
			if n, err := s.Write([]byte(filler)); n != len(filler) || err != nil {
				t.Fatalf("Write = %d, %v: a write past the bound must still be consumed", n, err)
			}
		}
		if n, err := s.Write([]byte(lfsRule)); n != len(lfsRule) || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
		if s.Found() {
			t.Error("a rule beyond maxAttributesBytes was read")
		}
		if s.total > maxAttributesBytes {
			t.Errorf("inspected %d bytes, want at most %d", s.total, maxAttributesBytes)
		}
	})
	t.Run("memory does not grow with a very long line", func(t *testing.T) {
		var s attributesScanner
		s.Write([]byte(strings.Repeat("a", maxAttributesBytes)))
		if cap(s.line) > 2*maxAttributesLine {
			t.Errorf("line buffer grew to %d bytes", cap(s.line))
		}
	})
}

func TestParseAttributesEntry(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    string
		wantErr error
	}{
		{"a regular file", lsTreeEntry("100644", "blob", lfsSHA1, "45"), lfsSHA1, nil},
		{"an executable file", lsTreeEntry("100755", "blob", lfsSHA1, "45"), lfsSHA1, nil},
		{"a sha-256 object", lsTreeEntry("100644", "blob", lfsSHA256, "45"), lfsSHA256, nil},
		{"the largest size read", lsTreeEntry("100644", "blob", lfsSHA1, "1048576"), lfsSHA1, nil},
		{"nothing listed", "", "", errAttributesAbsent},
		{"a symlink", lsTreeEntry("120000", "blob", lfsSHA1, "9"), "", errAttributesAbsent},
		{"a directory", lsTreeEntry("040000", "tree", lfsSHA1, "-"), "", errAttributesAbsent},
		{"a submodule", lsTreeEntry("160000", "commit", lfsSHA1, "-"), "", errAttributesAbsent},
		{"over the size read", lsTreeEntry("100644", "blob", lfsSHA1, "1048577"), "", errAttributesTooLarge},
		{"a huge size", lsTreeEntry("100644", "blob", lfsSHA1, "99999999999"), "", errAttributesTooLarge},
		{"a size that overflows", lsTreeEntry("100644", "blob", lfsSHA1, "99999999999999999999999"), "", errAttributesMalformed},
		{"a negative size", lsTreeEntry("100644", "blob", lfsSHA1, "-1"), "", errAttributesMalformed},
		{"an unknown mode", lsTreeEntry("100664", "blob", lfsSHA1, "45"), "", errAttributesMalformed},
		{"a regular mode on a tree", lsTreeEntry("100644", "tree", lfsSHA1, "45"), "", errAttributesMalformed},
		{"an uppercase object", lsTreeEntry("100644", "blob", strings.ToUpper(lfsSHA1), "45"), "", errAttributesMalformed},
		{"a short object", lsTreeEntry("100644", "blob", lfsSHA1[:39], "45"), "", errAttributesMalformed},
		{"an option as the object", lsTreeEntry("100644", "blob", "--"+lfsSHA1[2:], "45"), "", errAttributesMalformed},
		{"a different path", "100644 blob " + lfsSHA1 + "      45\tother\x00", "", errAttributesMalformed},
		{"the path with a prefix", "100644 blob " + lfsSHA1 + "      45\tdir/.gitattributes\x00", "", errAttributesMalformed},
		{"two entries", lsTreeEntry("100644", "blob", lfsSHA1, "45") + lsTreeEntry("100644", "blob", lfsSHA1, "45"), "", errAttributesMalformed},
		{"no terminator", strings.TrimSuffix(lsTreeEntry("100644", "blob", lfsSHA1, "45"), "\x00"), "", errAttributesMalformed},
		{"a lone terminator", "\x00", "", errAttributesMalformed},
		{"no tab", "100644 blob " + lfsSHA1 + " 45 .gitattributes\x00", "", errAttributesMalformed},
		{"a missing field", "100644 blob " + lfsSHA1 + "\t.gitattributes\x00", "", errAttributesMalformed},
		{"truncated output", "100644 blob 5dcf\n[... output truncated ...]\n", "", errAttributesMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAttributesEntry([]byte(tt.out))
			if err != tt.wantErr || got != tt.want {
				t.Errorf("parseAttributesEntry = %q, %v; want %q, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestValidObjectID(t *testing.T) {
	for id, want := range map[string]bool{
		lfsSHA1: true, lfsSHA256: true, "": false, "g" + lfsSHA1[1:]: false, lfsSHA1 + "0": false,
		strings.Repeat("0", 40): true, strings.Repeat("A", 40): false, "-" + lfsSHA1[1:]: false,
	} {
		if got := validObjectID(id); got != want {
			t.Errorf("validObjectID(%q) = %v, want %v", id, got, want)
		}
	}
}

// lfsGit fakes a healthy sync of a repository whose default branch is "trunk" and answers the two LFS
// lookups with the given output.
func lfsGit(lsTree, catFile string) *fakeGit {
	return &fakeGit{stdout: func(c gitCall) string {
		switch c.sub() {
		case "push":
			return pushedTwo
		case "ls-remote":
			return headIsTrunk
		case "ls-tree":
			return lsTree
		case "cat-file":
			return catFile
		}
		return ""
	}}
}

func TestSyncRepoReportsLFSWhenTheDefaultBranchAssignsTheFilter(t *testing.T) {
	f := lfsGit(lsTreeEntry("100644", "blob", lfsSHA1, "45"), lfsRule)
	d := newStatsDeps(t, f)
	got := syncRepo(context.Background(), d, testTask())

	if got.State != StateOK || got.ErrorType != "" {
		t.Fatalf("result = %+v, want ok", got)
	}
	if want := []string{WarnLFSNotReplicated}; !slices.Equal(got.Warnings, want) {
		t.Errorf("Warnings = %v, want %v", got.Warnings, want)
	}

	dir := "--git-dir=" + d.mirrors.Dir("repo-a")
	if want := []string{"init", "fetch", "show-ref", "for-each-ref", "push", "ls-remote", "ls-tree", "cat-file"}; !slices.Equal(f.subs(), want) {
		t.Fatalf("git sequence = %v, want %v", f.subs(), want)
	}
	lsTree, catFile := f.calls[6], f.calls[7]
	if want := []string{"git", dir, "ls-tree", "-l", "-z", "refs/heads/trunk", "--", ".gitattributes"}; !slices.Equal(lsTree.argv, want) {
		t.Errorf("ls-tree argv = %v, want %v", lsTree.argv, want)
	}
	if want := []string{"git", dir, "cat-file", "blob", lfsSHA1}; !slices.Equal(catFile.argv, want) {
		t.Errorf("cat-file argv = %v, want %v", catFile.argv, want)
	}
	for _, c := range []gitCall{lsTree, catFile} {
		if c.envHas(testOriginTok) || c.envHas(testPushTok) || c.envHas("extraHeader") || c.envHas(testOriginURL) || c.envHas(testReplicaURL) {
			t.Errorf("%s is local and must carry no credential or remote: %v", c.sub(), c.env)
		}
		if !slices.Contains(c.env, "GIT_NO_REPLACE_OBJECTS=1") || !slices.Contains(c.env, "GIT_CONFIG_NOSYSTEM=1") {
			t.Errorf("%s env = %v, want the locked-down base plus GIT_NO_REPLACE_OBJECTS=1", c.sub(), c.env)
		}
	}
}

func TestSyncRepoReportsNoLFSWarningWhenThereIsNoRule(t *testing.T) {
	tests := []struct {
		name         string
		lsTree       string
		catFile      string
		wantCatFile  bool
		noDefaultRef bool
		undecided    bool
	}{
		{"no .gitattributes", "", "", false, false, false},
		{"rules without the filter", lsTreeEntry("100644", "blob", lfsSHA1, "20"), "* text=auto\n*.sh eol=lf\n", true, false, false},
		{"only a commented rule", lsTreeEntry("100644", "blob", lfsSHA1, "20"), "# *.psd filter=lfs\n", true, false, false},
		{"a symlink named .gitattributes", lsTreeEntry("120000", "blob", lfsSHA1, "9"), lfsRule, false, false, false},
		{"a directory named .gitattributes", lsTreeEntry("040000", "tree", lfsSHA1, "-"), lfsRule, false, false, false},
		{"a file too large to read", lsTreeEntry("100644", "blob", lfsSHA1, "5000000"), lfsRule, false, false, true},
		{"unparseable ls-tree output", "garbage", lfsRule, false, false, true},
		{"ls-tree output beyond the cap", strings.Repeat("x", 2*maxLsTreeOutput), lfsRule, false, false, true},
		{"the default branch is unknown", lsTreeEntry("100644", "blob", lfsSHA1, "45"), lfsRule, false, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := lfsGit(tt.lsTree, tt.catFile)
			if tt.noDefaultRef {
				inner := f.stdout
				f.stdout = func(c gitCall) string {
					if c.sub() == "ls-remote" {
						return "5dcfa042fb9b025599cd26145fd324dce4651d5d\tHEAD\n" // a detached HEAD
					}
					return inner(c)
				}
			}
			got := syncRepo(context.Background(), newStatsDeps(t, f), testTask())

			if got.State != StateOK || got.Warnings != nil {
				t.Errorf("result = %+v, want ok with no warnings", got)
			}
			if got.WarningsUndecided != tt.undecided {
				t.Errorf("WarningsUndecided = %v, want %v", got.WarningsUndecided, tt.undecided)
			}
			if gotCat := slices.Contains(f.subs(), "cat-file"); gotCat != tt.wantCatFile {
				t.Errorf("cat-file ran = %v, want %v (sequence %v)", gotCat, tt.wantCatFile, f.subs())
			}
			if tt.noDefaultRef && slices.Contains(f.subs(), "ls-tree") {
				t.Errorf("sequence = %v: with no default branch there is nothing to look up", f.subs())
			}
		})
	}
}

func TestSyncRepoLFSDetectionFailureNeverFailsTheSync(t *testing.T) {
	for _, failing := range []string{"ls-tree", "cat-file"} {
		t.Run(failing, func(t *testing.T) {
			f := lfsGit(lsTreeEntry("100644", "blob", lfsSHA1, "45"), lfsRule)
			f.script = func(c gitCall) ([]byte, error) {
				if c.sub() == failing {
					return []byte("fatal: bad object refs/heads/trunk"), errExit
				}
				return nil, nil
			}
			got := syncRepo(context.Background(), newStatsDeps(t, f), testTask())
			if got.State != StateOK || got.ErrorType != "" || got.ErrorDetail != "" || got.Warnings != nil || !got.WarningsUndecided {
				t.Errorf("result = %+v, want an ok that did not decide its warnings", got)
			}
			if got.Stats.RefsPushed == nil || *got.Stats.RefsPushed != 2 {
				t.Errorf("RefsPushed = %v, want the push's 2 regardless", got.Stats.RefsPushed)
			}
		})
	}
}

func TestSyncRepoLFSDetectionTimeoutIsNotAFailure(t *testing.T) {
	d := newStatsDeps(t, lfsGit("", ""))
	// The lookup waits for its context, as a wedged git would.
	d.runOut = func(ctx context.Context, argv, env []string, dir string, w io.Writer) ([]byte, error) {
		if slices.Contains(argv, "ls-tree") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	found, reason := d.usesLFS(ctx, t.TempDir(), testTask(), "trunk")
	if found || reason != lfsReasonTimeout {
		t.Errorf("usesLFS = %v, %q, want no verdict by timeout", found, reason)
	}
}

func TestSyncRepoPushFailureCarriesNoWarningAndSkipsTheLookup(t *testing.T) {
	f := lfsGit(lsTreeEntry("100644", "blob", lfsSHA1, "45"), lfsRule)
	f.script = func(c gitCall) ([]byte, error) {
		if c.sub() == "push" {
			return []byte("fatal: unable to access 'https://replica.example.com/': The requested URL returned error: 503"), errExit
		}
		return nil, nil
	}
	got := syncRepo(context.Background(), newStatsDeps(t, f), testTask())
	if got.State != StateError || got.Warnings != nil {
		t.Errorf("result = %+v, want an error without warnings", got)
	}
	if slices.Contains(f.subs(), "ls-tree") {
		t.Errorf("sequence = %v: a failed sync must not look for LFS", f.subs())
	}
}

func TestSyncRepoWithoutStatisticsDoesNotLookForLFS(t *testing.T) {
	f := lfsGit(lsTreeEntry("100644", "blob", lfsSHA1, "45"), lfsRule)
	got := syncRepo(context.Background(), newTestDeps(t, f), testTask())
	if got.State != StateOK || got.Warnings != nil || !got.WarningsUndecided || slices.Contains(f.subs(), "ls-tree") {
		t.Errorf("result = %+v, sequence = %v, want the original sequence with no lookup", got, f.subs())
	}
}

// syncRepo stops looking once the sync is cancelled: nothing is recorded for a cancelled sync anyway.
func TestDetectLFSDoesNothingOnceCancelled(t *testing.T) {
	f := lfsGit(lsTreeEntry("100644", "blob", lfsSHA1, "45"), lfsRule)
	d := newStatsDeps(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, decided := d.detectLFS(ctx, t.TempDir(), testTask(), "trunk"); got != nil || decided || len(f.calls) != 0 {
		t.Errorf("detectLFS = %v, %v after %d git calls, want nothing and undecided", got, decided, len(f.calls))
	}
}

// Nothing git or the origin said may reach the log: not its output, a ref name, nor a path.
func TestLFSDetectionLogsOnlyFixedWords(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const hostileBranch = "hostile-branch-name"
	f := lfsGit("", "")
	f.script = func(c gitCall) ([]byte, error) {
		if c.sub() == "ls-tree" {
			return []byte("fatal: not a tree object refs/heads/" + hostileBranch + " /srv/hostile/path"), errExit
		}
		return nil, nil
	}
	f.stdout = func(c gitCall) string {
		if c.sub() == "ls-remote" {
			return "ref: refs/heads/" + hostileBranch + "\tHEAD\n"
		}
		return ""
	}
	syncRepo(context.Background(), newStatsDeps(t, f), testTask())

	log := buf.String()
	if !strings.Contains(log, "repo_id=repo-a") || !strings.Contains(log, "reason="+lfsReasonGit) {
		t.Errorf("log = %q, want the repo id and the fixed reason", log)
	}
	for _, leaked := range []string{hostileBranch, "/srv/hostile", "not a tree object", "refs/heads"} {
		if strings.Contains(log, leaked) {
			t.Errorf("log %q leaks %q", log, leaked)
		}
	}
}

// lfsOrigin is a synthetic origin repository for tests against real git.
type lfsOrigin struct {
	t      *testing.T
	bare   string
	work   string
	mirror Mirrors
	deps   syncDeps
	task   syncTask
}

func newLFSOrigin(t *testing.T, defaultBranch string) *lfsOrigin {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	o := &lfsOrigin{t: t, bare: filepath.Join(root, "origin.git"), work: filepath.Join(root, "work")}
	replica := filepath.Join(root, "replica.git")
	git(t, "", "init", "--bare", "-b", defaultBranch, o.bare)
	git(t, "", "init", "--bare", "-b", "main", replica)
	git(t, "", "init", "-b", defaultBranch, o.work)
	o.mirror = Mirrors{Root: filepath.Join(root, "mirrors")}
	o.deps = syncDeps{mirrors: o.mirror, run: runGit, runOut: runGitOut, now: time.Now}
	o.task = syncTask{
		RepoID:       "repo-a",
		CanonicalURL: "file://" + o.bare,
		ReplicaURL:   "file://" + replica,
		PushToken:    Secret("push-token-abcdefghij"),
	}
	return o
}

// commit writes files (path to content) on the checked-out branch, removes the paths in remove, and
// commits.
func (o *lfsOrigin) commit(files map[string]string, remove ...string) {
	o.t.Helper()
	for path, content := range files {
		full := filepath.Join(o.work, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			o.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			o.t.Fatal(err)
		}
	}
	for _, path := range remove {
		if err := os.Remove(filepath.Join(o.work, path)); err != nil {
			o.t.Fatal(err)
		}
	}
	git(o.t, o.work, "add", "--all")
	git(o.t, o.work, "commit", "--allow-empty", "-m", "change")
}

func (o *lfsOrigin) push(branches ...string) {
	o.t.Helper()
	git(o.t, o.work, append([]string{"push", "--force", o.bare}, branches...)...)
}

func (o *lfsOrigin) sync() syncResult {
	o.t.Helper()
	res := syncRepo(context.Background(), o.deps, o.task)
	if res.State != StateOK {
		o.t.Fatalf("sync = %+v, want ok", res)
	}
	return res
}

func TestSyncRepoDetectsLFSAgainstRealGit(t *testing.T) {
	tests := []struct {
		name  string
		setup func(o *lfsOrigin)
		want  []string
	}{
		{
			name: "a rule written by git lfs track",
			setup: func(o *lfsOrigin) {
				o.commit(map[string]string{".gitattributes": lfsRule, "a.txt": "a"})
				o.push("main")
			},
			want: []string{WarnLFSNotReplicated},
		},
		{
			name: "a rule among other attributes, with crlf line ends",
			setup: func(o *lfsOrigin) {
				o.commit(map[string]string{".gitattributes": "* text=auto\r\n*.bin filter=lfs -text\r\n"})
				o.push("main")
			},
			want: []string{WarnLFSNotReplicated},
		},
		{
			name:  "no .gitattributes at all",
			setup: func(o *lfsOrigin) { o.commit(map[string]string{"a.txt": "a"}); o.push("main") },
		},
		{
			name: "attributes without the filter",
			setup: func(o *lfsOrigin) {
				o.commit(map[string]string{".gitattributes": "* text=auto\n*.png binary\n"})
				o.push("main")
			},
		},
		{
			name:  "a rule only in a nested .gitattributes is not examined",
			setup: func(o *lfsOrigin) { o.commit(map[string]string{"assets/.gitattributes": lfsRule}); o.push("main") },
		},
		{
			name: "a .gitattributes that is a symlink is not read, as git does not read it",
			setup: func(o *lfsOrigin) {
				if err := os.Symlink("elsewhere", filepath.Join(o.work, ".gitattributes")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				o.commit(map[string]string{"elsewhere": lfsRule})
				o.push("main")
			},
		},
		{
			name: "a rule only on another branch is not examined",
			setup: func(o *lfsOrigin) {
				o.commit(map[string]string{"a.txt": "a"})
				git(t, o.work, "checkout", "-b", "feature")
				o.commit(map[string]string{".gitattributes": lfsRule})
				o.push("main", "feature")
			},
		},
		{
			name: "the default branch is the one examined, whatever its name",
			setup: func(o *lfsOrigin) {
				o.commit(map[string]string{"a.txt": "a"})
				git(t, o.work, "checkout", "-b", "develop")
				o.commit(map[string]string{".gitattributes": lfsRule})
				o.push("main", "develop")
				git(t, "", "--git-dir="+o.bare, "symbolic-ref", "HEAD", "refs/heads/develop")
			},
			want: []string{WarnLFSNotReplicated},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newLFSOrigin(t, "main")
			tt.setup(o)
			got := o.sync()
			if !slices.Equal(got.Warnings, tt.want) {
				t.Errorf("Warnings = %v, want %v", got.Warnings, tt.want)
			}
		})
	}
}

// The warning is recomputed by every sync: it clears once the default branch stops assigning the
// filter, and comes back if the rule does.
func TestSyncRepoLFSWarningClearsWhenTheRuleIsRemoved(t *testing.T) {
	o := newLFSOrigin(t, "main")
	o.commit(map[string]string{".gitattributes": lfsRule})
	o.push("main")
	if got := o.sync(); !slices.Equal(got.Warnings, []string{WarnLFSNotReplicated}) {
		t.Fatalf("first sync Warnings = %v, want the LFS warning", got.Warnings)
	}

	o.commit(map[string]string{".gitattributes": "* text=auto\n"})
	o.push("main")
	if got := o.sync(); got.Warnings != nil {
		t.Errorf("after the rule was replaced, Warnings = %v, want none", got.Warnings)
	}

	o.commit(nil, ".gitattributes")
	o.push("main")
	if got := o.sync(); got.Warnings != nil {
		t.Errorf("after .gitattributes was deleted, Warnings = %v, want none", got.Warnings)
	}

	o.commit(map[string]string{".gitattributes": lfsRule})
	o.push("main")
	if got := o.sync(); !slices.Equal(got.Warnings, []string{WarnLFSNotReplicated}) {
		t.Errorf("after the rule came back, Warnings = %v, want the LFS warning", got.Warnings)
	}
}

// A file bigger than the bound is never read, but the sync is unaffected.
func TestSyncRepoOversizeAttributesAreNotRead(t *testing.T) {
	o := newLFSOrigin(t, "main")
	o.commit(map[string]string{".gitattributes": strings.Repeat("# padding\n", maxAttributesBytes/len("# padding\n")+10) + lfsRule})
	o.push("main")
	if got := o.sync(); got.Warnings != nil {
		t.Errorf("Warnings = %v, want none for a file over the bound", got.Warnings)
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{max: 4}
	for _, in := range []string{"ab", "cd"} {
		if n, err := b.Write([]byte(in)); n != len(in) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", in, n, err)
		}
	}
	if string(b.buf) != "abcd" || b.over {
		t.Fatalf("buf = %q over = %v, want abcd within the cap", b.buf, b.over)
	}
	if n, err := b.Write([]byte("efgh")); n != 4 || err != nil {
		t.Fatalf("Write past the cap = %d, %v, want it consumed", n, err)
	}
	if string(b.buf) != "abcd" || !b.over {
		t.Errorf("buf = %q over = %v, want the first 4 bytes and the overflow noted", b.buf, b.over)
	}
}
