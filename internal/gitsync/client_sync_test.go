package gitsync_test

import (
	"context"
	"strings"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

func TestApplyConfigRetainsReplicaMaterialForMatched(t *testing.T) {
	c := newTestClient(t, oneProvider)
	c.HandleMessage(context.Background(), &stubSender{}, "git_sync_config", configJSONRaw("1",
		repoJSON("repo-a", "https://git.example.com/team/a.git", "", "https://REPLICA.example.com/r/a.git", testPushToken),
		repoJSON("repo-b", "https://other.example.com/b.git", "", "https://replica.example.com/r/b.git", testPushToken)))

	replica, token, found := c.ReplicaMaterialForTest("repo-a")
	if !found {
		t.Fatal("repo-a not retained")
	}
	if want := "https://replica.example.com:443/r/a.git"; replica != want {
		t.Errorf("replica URL = %q, want the validated canonical form %q", replica, want)
	}
	if token != testPushToken {
		t.Error("the push token was not retained for a matched repository")
	}

	// A repository that cannot be synced must not hold a push token it will never use.
	replica, token, found = c.ReplicaMaterialForTest("repo-b")
	if !found || replica != "" || token != "" {
		t.Errorf("unmatched repo-b retained replica=%q token-present=%v, want neither", replica, token != "")
	}
}

func TestApplyConfigMalformedReplicaMarksRepoNotWholeConfig(t *testing.T) {
	c := newTestClient(t, oneProvider)
	s := &stubSender{}
	c.HandleMessage(context.Background(), s, "git_sync_config", configJSONRaw("5",
		repoJSON("good", "https://git.example.com/g.git", "", "https://replica.example.com/r/g.git", testPushToken),
		repoJSON("plain-http", "https://git.example.com/h.git", "", "http://replica.example.com/r/h.git", testPushToken),
		repoJSON("no-replica", "https://git.example.com/n.git", "", "", testPushToken),
		repoJSON("no-token", "https://git.example.com/t.git", "", "https://replica.example.com/r/t.git", ""),
		repoJSON("userinfo", "https://git.example.com/u.git", "", "https://user:pw@replica.example.com/r/u.git", testPushToken)))

	st := lastStatus(t, s.messages())
	if string(st.ConfigVersion) != "5" {
		t.Fatalf("config_version = %s, want 5: one bad repository must not reject the whole config", st.ConfigVersion)
	}
	rs := repoStates(st)
	if rs["good"].State != gitsync.StatePending {
		t.Errorf("good = %+v, want pending", rs["good"])
	}
	for _, id := range []string{"plain-http", "no-replica", "no-token", "userinfo"} {
		r := rs[id]
		if r.State != gitsync.StateError || r.ErrorType != gitsync.ErrUnclassified || r.ErrorDetail != "invalid replica configuration" {
			t.Errorf("%s = %+v, want error/unclassified/invalid replica configuration", id, r)
		}
		if strings.Contains(r.ErrorDetail, "replica.example.com") {
			t.Errorf("%s: error_detail leaked the replica host", id)
		}
		if replica, _, _ := c.ReplicaMaterialForTest(id); replica != "" {
			t.Errorf("%s retained an unusable replica URL %q", id, replica)
		}
	}
}

func TestApplyConfigCarriesForwardResultForUnchangedRepo(t *testing.T) {
	apply := func(c *gitsync.Client, origin, replica string) proto.GitSyncRepoStatus {
		s := &stubSender{}
		c.HandleMessage(context.Background(), s, "git_sync_config", configJSONRaw("1",
			repoJSON("repo-a", origin, "", replica, testPushToken)))
		return repoStates(lastStatus(t, s.messages()))["repo-a"]
	}
	const origin, replica = "https://git.example.com/a.git", "https://replica.example.com/r/a.git"

	c := newTestClient(t, oneProvider)
	apply(c, origin, replica)
	c.RecordResultForTest("repo-a", gitsync.StateOK, "")

	if got := apply(c, origin, replica); got.State != gitsync.StateOK {
		t.Errorf("an unchanged repository was reset to %q; want its prior ok result carried forward", got.State)
	}
	if got := apply(c, "https://git.example.com/other.git", replica); got.State != gitsync.StatePending {
		t.Errorf("a changed origin kept a stale result: %+v, want pending", got)
	}

	c.RecordResultForTest("repo-a", gitsync.StateOK, "")
	if got := apply(c, "https://git.example.com/other.git", "https://replica.example.com/r/moved.git"); got.State != gitsync.StatePending {
		t.Errorf("a changed replica kept a stale result: %+v, want pending", got)
	}
}
