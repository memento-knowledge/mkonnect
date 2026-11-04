package gitsync_test

import (
	"os"
	"strings"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

// TestParseReplicaHostMatchesChartTable keeps the connector's own check of GIT_SYNC_REPLICA_HOST in
// agreement with the verdict table the chart's render-time validation is tested against, so a value
// the chart accepts is never one the connector would treat as not pinned (and vice versa).
func TestParseReplicaHostMatchesChartTable(t *testing.T) {
	data, err := os.ReadFile("../../scripts/testdata/gitsync-replica-hosts.txt")
	if err != nil {
		t.Fatalf("read the shared verdict table: %v", err)
	}
	checked := 0
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		verdict, value, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("malformed table line %q", line)
		}
		_, err := gitsync.ParseReplicaHost(value)
		switch {
		case verdict == "ACCEPT" && err != nil:
			t.Errorf("table accepts %q but ParseReplicaHost rejects it: %v", value, err)
		case verdict == "reject" && err == nil:
			t.Errorf("table rejects %q but ParseReplicaHost accepts it", value)
		}
		checked++
	}
	if checked < 50 {
		t.Fatalf("only %d table rows checked; the table moved or is truncated", checked)
	}
}
