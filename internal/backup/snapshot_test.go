package backup

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

func TestSelectSnapshot(t *testing.T) {
	const (
		old1   = "/data/partitions/20261104/snapshots/20261104091028-18C876EFEB03599A"
		other  = "/data/partitions/20261103/snapshots/20261105094918-18E51D0B85C98DAD"
		new1   = "/data/partitions/20261104/snapshots/20261105094918-18E51D0B85C98DAF"
		new2   = "/data/partitions/20261104/snapshots/20261105094919-18E51D0B85C98DB3"
		winNew = `C:\vl\partitions\20261104\snapshots\20261105094918-18E51D0B85C98DAF`
	)

	tests := []struct {
		name                   string
		before, after, created []string
		want                   string
		wantOthers             []string
		wantErr                bool
	}{
		{
			name:   "found by list diff",
			before: []string{old1, other},
			after:  []string{old1, other, new1},
			want:   new1,
		},
		{
			name:    "found by create response only",
			before:  []string{old1},
			after:   nil,
			created: []string{new1},
			want:    new1,
		},
		{
			name:    "list diff and create response agree",
			before:  nil,
			after:   []string{new1},
			created: []string{new1},
			want:    new1,
		},
		{
			name:   "other partitions are ignored",
			before: nil,
			after:  []string{other},
			// A new snapshot of 20261103 must not be picked for 20261104.
			wantErr: true,
		},
		{
			name:       "several candidates: newest wins",
			before:     []string{old1},
			after:      []string{new2, old1, new1},
			want:       new2,
			wantOthers: []string{new1},
		},
		{
			name:    "nothing new",
			before:  []string{old1},
			after:   []string{old1},
			created: []string{},
			wantErr: true,
		},
		{
			name:  "windows separators",
			after: []string{winNew},
			want:  winNew,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, others, err := selectSnapshot(tt.before, tt.after, tt.created, "20261104")
			if tt.wantErr {
				if !errors.Is(err, ErrNoSnapshot) {
					t.Fatalf("expected ErrNoSnapshot; got %v (selected %q)", err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if got != tt.want {
				t.Fatalf("selected %q; want %q", got, tt.want)
			}
			if !slices.Equal(others, tt.wantOthers) {
				t.Fatalf("others = %q; want %q", others, tt.wantOthers)
			}
		})
	}
}

func TestLocalSnapshotDir(t *testing.T) {
	storage := filepath.FromSlash("/home/u/VictoriaLogsData")
	const name = "20261104091028-18C876EFEB03599A"
	want := filepath.Join(storage, "partitions", "20261104", "snapshots", name)

	good := []string{
		"/data/partitions/20261104/snapshots/" + name,
		"/data/partitions/20261104/snapshots/" + name + "/",
		"/var/lib/vl/partitions/x/partitions/20261104/snapshots/" + name, // last /partitions/ wins
		"/home/u/VictoriaLogsData/partitions/20261104/snapshots/" + name,
		`D:\vl\partitions\20261104\snapshots\` + name,
	}
	for _, p := range good {
		dir, gotName, err := localSnapshotDir(storage, p, "20261104")
		if err != nil {
			t.Errorf("localSnapshotDir(%q): unexpected error: %s", p, err)
			continue
		}
		if dir != want || gotName != name {
			t.Errorf("localSnapshotDir(%q) = %q, %q; want %q, %q", p, dir, gotName, want, name)
		}
	}

	bad := []string{
		"",
		"/data/20261104/snapshots/" + name,
		"/data/partitions/20261103/snapshots/" + name, // wrong date
		"/data/partitions/20261104/snapshot/" + name,
		"/data/partitions/20261104/snapshots/",
		"/data/partitions/20261104/snapshots/..",
		"/data/partitions/20261104/snapshots/./",
		"/data/partitions/20261104/snapshots/" + name + "/datadb",
		"partitions/20261104/snapshots/" + name, // no leading slash before partitions
	}
	for _, p := range bad {
		if dir, _, err := localSnapshotDir(storage, p, "20261104"); err == nil {
			t.Errorf("localSnapshotDir(%q) = %q; expected an error", p, dir)
		}
	}
}
