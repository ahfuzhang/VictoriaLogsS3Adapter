package vlclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestClient(t *testing.T) {
	var lastQuery map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastQuery = map[string]string{}
		for k := range r.URL.Query() {
			lastQuery[k] = r.URL.Query().Get(k)
		}
		switch r.URL.Path {
		case "/health":
			w.Write([]byte("OK"))
		case "/internal/partition/snapshot/list":
			w.Write([]byte(`["/data/partitions/20261104/snapshots/A"]`))
		case "/internal/partition/snapshot/create":
			w.Write([]byte(`"/data/partitions/20261104/snapshots/B"`))
		case "/internal/partition/list":
			w.Write([]byte(`[]`))
		case "/internal/partition/attach":
			http.Error(w, "cannot attach", http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(srv.URL+"/", "secret")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := lastQuery["authKey"]; ok {
		t.Fatal("authKey must not be sent to /health")
	}

	list, err := c.SnapshotList(ctx)
	if err != nil || !reflect.DeepEqual(list, []string{"/data/partitions/20261104/snapshots/A"}) {
		t.Fatalf("SnapshotList = %v, %v", list, err)
	}
	if lastQuery["authKey"] != "secret" {
		t.Fatal("authKey must be sent to /internal/*")
	}

	created, err := c.SnapshotCreate(ctx, "20261104")
	if err != nil || !reflect.DeepEqual(created, []string{"/data/partitions/20261104/snapshots/B"}) {
		t.Fatalf("SnapshotCreate = %v, %v", created, err)
	}
	if lastQuery["partition_prefix"] != "20261104" || lastQuery["name"] != "20261104" {
		t.Fatalf("unexpected create args: %v", lastQuery)
	}

	pts, err := c.PartitionList(ctx)
	if err != nil || len(pts) != 0 {
		t.Fatalf("PartitionList = %v, %v", pts, err)
	}

	err = c.PartitionAttach(ctx, "20261104")
	if !IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("expected 400 error; got %v", err)
	}
	err = c.SnapshotDelete(ctx, "/x")
	if !IsStatus(err, http.StatusNotFound) {
		t.Fatalf("expected 404 error; got %v", err)
	}
}

func TestParseStringList(t *testing.T) {
	cases := map[string][]string{
		``:          nil,
		`null`:      nil,
		`""`:        nil,
		`"a"`:       {"a"},
		`["a","b"]`: {"a", "b"},
		`[]`:        {},
	}
	for in, want := range cases {
		got, err := parseStringList([]byte(in))
		if err != nil {
			t.Fatalf("parseStringList(%q): %s", in, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("parseStringList(%q) = %#v; want %#v", in, got, want)
		}
	}
	if _, err := parseStringList([]byte(`{"a":1}`)); err == nil {
		t.Fatal("expected error")
	}
}
