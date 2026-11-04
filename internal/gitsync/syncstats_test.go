package gitsync

import (
	"io"
	"strings"
	"testing"
	"time"
)

func feed(t *testing.T, w io.Writer, chunks ...string) {
	t.Helper()
	for _, c := range chunks {
		if n, err := io.WriteString(w, c); err != nil || n != len(c) {
			t.Fatalf("Write(%q) = %d, %v; a stats writer must consume everything it is given", c, n, err)
		}
	}
}

func TestPushCounterCountsOnlyRefsThatChangedOnTheReplica(t *testing.T) {
	tests := []struct {
		name         string
		out          string
		wantRefs     int
		wantRejected int
	}{
		{"nothing", "", 0, 0},
		{"new branch and tag", "To https://replica.example.com/r.git\n*\trefs/heads/main:refs/heads/main\t[new branch]\n*\trefs/tags/v1:refs/tags/v1\t[new tag]\nDone\n", 2, 0},
		{"fast-forward", " \trefs/heads/main:refs/heads/main\t1111111..2222222\nDone\n", 1, 0},
		{"forced update", "+\trefs/heads/main:refs/heads/main\t1111111...2222222 (forced update)\nDone\n", 1, 0},
		{"deleted", "-\t:refs/heads/old\t[deleted]\nDone\n", 1, 0},
		{"up to date is not counted", "=\trefs/heads/main:refs/heads/main\t[up to date]\nDone\n", 0, 0},
		{"rejected is not counted", "!\trefs/heads/main:refs/heads/main\t[remote rejected] (hook declined)\nDone\n", 0, 1},
		{"mixed", "To x\n=\ta:a\t[up to date]\n*\tb:b\t[new branch]\n \tc:c\t1..2\n+\td:d\t1...2 (forced update)\n-\t:e\t[deleted]\n!\tf:f\t[rejected] (non-fast-forward)\nDone\n", 4, 1},
		{"header and footer lines never count", "To https://replica.example.com/r.git\nDone\n", 0, 0},
		{"a flag without the tab is not a status line", "*refs/heads/x\n -x\n", 0, 0},
		{"final line without newline", "*\ta:a\t[new branch]", 1, 0},
		{"windows line endings do not matter", "*\ta:a\t[new branch]\r\n*\tb:b\t[new branch]\r\n", 2, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p pushCounter
			feed(t, &p, tc.out)
			if p.Refs() != tc.wantRefs || p.Rejected() != tc.wantRejected {
				t.Errorf("refs, rejected = %d, %d; want %d, %d", p.Refs(), p.Rejected(), tc.wantRefs, tc.wantRejected)
			}
		})
	}
}

func TestPushCounterToleratesAnyChunking(t *testing.T) {
	const out = "To x\n*\ta:a\t[new branch]\n!\tb:b\t[remote rejected] (hook declined)\n+\tc:c\t1...2 (forced update)\nDone\n"
	var whole pushCounter
	feed(t, &whole, out)

	var bytewise pushCounter
	for i := range len(out) {
		feed(t, &bytewise, out[i:i+1])
	}
	if bytewise.Refs() != whole.Refs() || bytewise.Rejected() != whole.Rejected() ||
		string(bytewise.RejectionText()) != string(whole.RejectionText()) {
		t.Errorf("byte-at-a-time = (%d, %d, %q), whole = (%d, %d, %q)",
			bytewise.Refs(), bytewise.Rejected(), bytewise.RejectionText(), whole.Refs(), whole.Rejected(), whole.RejectionText())
	}
	if whole.Refs() != 2 || whole.Rejected() != 1 {
		t.Errorf("refs, rejected = %d, %d; want 2, 1", whole.Refs(), whole.Rejected())
	}
}

func TestPushCounterAccumulatesAcrossBatches(t *testing.T) {
	var p pushCounter
	feed(t, &p, "*\ta:a\t[new branch]\nDone\n")
	feed(t, &p, "*\tb:b\t[new branch]\n=\tc:c\t[up to date]\nDone\n")
	if p.Refs() != 2 {
		t.Errorf("refs = %d, want 2 across two pushes", p.Refs())
	}
}

func TestPushCounterNeverBuffersAnUnboundedLine(t *testing.T) {
	var p pushCounter
	feed(t, &p, "*\t")
	feed(t, &p, strings.Repeat("x", 10<<20))
	feed(t, &p, "\n!\t")
	feed(t, &p, strings.Repeat("y", 10<<20))
	feed(t, &p, "\n")
	if p.Refs() != 1 || p.Rejected() != 1 {
		t.Errorf("refs, rejected = %d, %d; want 1, 1", p.Refs(), p.Rejected())
	}
	if n := len(p.RejectionText()); n > maxRejectionText {
		t.Errorf("retained rejection text is %d bytes, want at most %d", n, maxRejectionText)
	}
}

// A rejected ref is retained in the classic non-porcelain shape, with fixed placeholder ref names, so
// the classifier (which reads git's stderr) sees what it always has and a ref name can never steer it.
func TestPushCounterRewritesRejectionsIntoTheClassicShape(t *testing.T) {
	var p pushCounter
	feed(t, &p, "!\trefs/heads/no-space-left-on-device:refs/heads/quota\t[remote rejected] (pre-receive hook declined)\n")
	got := string(p.RejectionText())
	if want := " ! ref -> ref [remote rejected] (pre-receive hook declined)\n"; got != want {
		t.Errorf("rejection text = %q, want %q", got, want)
	}
	if typ, _ := classifyGitFailure(replicaSide, errExit, p.RejectionText(), false); typ != ErrReplicaRejected {
		t.Errorf("classified as %q, want %q", typ, ErrReplicaRejected)
	}
}

// A rejection's explanation is the summary after the ref pair; a very long ref name must not push it
// out of what is retained, or a refused push would be classified as an unknown failure.
func TestPushCounterKeepsTheSummaryOfARejectionWithAVeryLongRefName(t *testing.T) {
	var p pushCounter
	long := strings.Repeat("a", 5000)
	feed(t, &p, "!\trefs/heads/"+long+":refs/heads/"+long+"\t[remote rejected] (push size limit exceeded)\n")
	if want := " ! ref -> ref [remote rejected] (push size limit exceeded)\n"; string(p.RejectionText()) != want {
		t.Errorf("rejection text = %q, want %q", p.RejectionText(), want)
	}
}

func TestNewestTrackerDropsTimesAfterItsCeiling(t *testing.T) {
	n := newestTracker{ceiling: 1700000000}
	feed(t, &n, "1600000000|\n4102444800|\n|1700000001\n1700000000|\n")
	if got, ok := n.Newest(); !ok || got.Unix() != 1700000000 {
		t.Errorf("Newest() = %v, %v; want 1700000000: later times are not credible", got, ok)
	}
}

func TestNewestTracker(t *testing.T) {
	const (
		jan2020 = 1577836800
		jan2021 = 1609459200
		max9999 = 253402300799
	)
	tests := []struct {
		name string
		out  string
		want int64 // 0: unknown
	}{
		{"empty", "", 0},
		{"branch and lightweight tag set the first field", "1577836800|\n1609459200|\n", jan2021},
		{"annotated tag sets only the peeled field", "|1609459200\n1577836800|\n", jan2021},
		{"a tag of a tree sets neither", "|\n1577836800|\n", jan2020},
		{"only unusable lines", "|\n\n", 0},
		{"garbage is skipped", "abc|\n-5|\n0|\n1577836800|\n", jan2020},
		{"beyond year 9999 is dropped", "253402300800|\n1577836800|\n", jan2020},
		{"year 9999 itself is kept", "253402300799|\n", max9999},
		{"no trailing newline", "1577836800|", jan2020},
		{"overlong line is skipped", strings.Repeat("9", 4096) + "|\n1577836800|\n", jan2020},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var n newestTracker
			feed(t, &n, tc.out)
			got, ok := n.Newest()
			if tc.want == 0 {
				if ok {
					t.Fatalf("Newest() = %v, true; want unknown", got)
				}
				return
			}
			if !ok || got.Unix() != tc.want || got.Location() != time.UTC {
				t.Errorf("Newest() = %v, %v; want %d UTC", got, ok, tc.want)
			}
		})
	}
}

func TestParseSymrefHead(t *testing.T) {
	tests := []struct {
		name, out, want string
	}{
		{"typical", "ref: refs/heads/main\tHEAD\n5dcfa042fb9b025599cd26145fd324dce4651d5d\tHEAD\n", "main"},
		{"slashed branch name", "ref: refs/heads/release/1.x\tHEAD\n", "release/1.x"},
		{"detached head has no symref line", "5dcfa042fb9b025599cd26145fd324dce4651d5d\tHEAD\n", ""},
		{"empty", "", ""},
		{"symref to something that is not a branch", "ref: refs/tags/v1\tHEAD\n", ""},
		{"symref for another ref", "ref: refs/heads/main\trefs/heads/alias\n", ""},
		{"control characters are refused", "ref: refs/heads/ma\x1bin\tHEAD\n", ""},
		{"empty name", "ref: refs/heads/\tHEAD\n", ""},
		// A hostile server can advertise any target: format characters that reorder or hide text, and
		// everything git itself forbids in a ref name, are refused.
		{"bidi override", "ref: refs/heads/ma\u202ein\tHEAD\n", ""},
		{"zero-width space", "ref: refs/heads/ma\u200bin\tHEAD\n", ""},
		{"space", "ref: refs/heads/my branch\tHEAD\n", ""},
		{"colon", "ref: refs/heads/a:b\tHEAD\n", ""},
		{"tilde", "ref: refs/heads/a~1\tHEAD\n", ""},
		{"double dot", "ref: refs/heads/a..b\tHEAD\n", ""},
		{"reflog syntax", "ref: refs/heads/a@{1}\tHEAD\n", ""},
		{"trailing slash", "ref: refs/heads/a/\tHEAD\n", ""},
		{"lock suffix", "ref: refs/heads/a.lock\tHEAD\n", ""},
		{"non-latin names are fine", "ref: refs/heads/főág\tHEAD\n", "főág"},
		{"invalid utf-8", "ref: refs/heads/\xff\xfe\tHEAD\n", ""},
		{"too long", "ref: refs/heads/" + strings.Repeat("a", 256) + "\tHEAD\n", ""},
		{"longest accepted", "ref: refs/heads/" + strings.Repeat("a", 255) + "\tHEAD\n", strings.Repeat("a", 255)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSymrefHead([]byte(tc.out))
			if ok != (tc.want != "") || got != tc.want {
				t.Errorf("parseSymrefHead(%q) = %q, %v; want %q", tc.out, got, ok, tc.want)
			}
		})
	}
}
