package restore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseRetention(t *testing.T) {
	const day = 24 * time.Hour
	ok := map[string]time.Duration{
		"7d":   7 * day,
		"1w":   7 * day,
		"100y": 100 * 365 * day,
		"12h":  12 * time.Hour,
		"1":    31 * day,
		"1.5":  time.Duration(1.5 * float64(31*day)),
		"2Y":   2 * 365 * day,
	}
	for in, want := range ok {
		got, err := parseRetention(in)
		if err != nil {
			t.Fatalf("parseRetention(%q): unexpected error: %s", in, err)
		}
		if got != want {
			t.Fatalf("parseRetention(%q): got %s; want %s", in, got, want)
		}
	}
	for _, in := range []string{"", "0", "-1", "5m", "abc", "-7d"} {
		if got, err := parseRetention(in); err == nil {
			t.Fatalf("parseRetention(%q): expecting error; got %s", in, got)
		}
	}
}

func TestParseRetentionFromMetrics(t *testing.T) {
	metrics := `vl_rows_ingested_total{type="jsonline"} 0
flag{name="futureRetention", value="2d", is_set="false"} 1
flag{name="partitionManageAuthKey", value="secret", is_set="false"} 1
flag{name="retentionPeriod", value="30d", is_set="true"} 1
flag{name="storageDataPath", value="/data", is_set="true"} 1
`
	got, err := parseRetentionFromMetrics(strings.NewReader(metrics))
	if err != nil {
		t.Fatal(err)
	}
	if got != 30*24*time.Hour {
		t.Fatalf("unexpected retention %s", got)
	}
	if _, err := parseRetentionFromMetrics(strings.NewReader("foo 1\n")); err == nil {
		t.Fatalf("expecting error when the flag metric is missing")
	}
}

func TestMinRetainedDay(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	got := minRetainedDay(now, 7*24*time.Hour)
	want, _ := dayNumber("20261029")
	if got != want {
		t.Fatalf("got day %d; want %d", got, want)
	}
}

func TestMarkUnattachedPartitions(t *testing.T) {
	env := newTestEnv(t, "")
	env.mu.Lock()
	env.attached = []string{"20261104"}
	env.mu.Unlock()
	r, storageDir := newTestRestorer(t, newFakeStore(), nil, nil)
	for _, day := range []string{"20261103", "20261104"} {
		if err := os.MkdirAll(filepath.Join(storageDir, "partitions", day), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	markUnattachedPartitions(context.Background(), env.proxy.currentBackend(), r)
	if !r.isUnloaded("20261103") {
		t.Fatalf("20261103 must be marked as unloaded")
	}
	if r.isUnloaded("20261104") {
		t.Fatalf("20261104 is attached and must not be marked as unloaded")
	}
}
