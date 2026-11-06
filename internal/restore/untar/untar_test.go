package untar

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name     string
	typ      byte
	body     string
	linkname string
}

func dirEntry(name string) entry        { return entry{name: name, typ: tar.TypeDir} }
func fileEntry(name, body string) entry { return entry{name: name, typ: tar.TypeReg, body: body} }

func makeTar(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Linkname: e.linkname}
		if e.typ == tar.TypeDir {
			hdr.Mode = 0o755
		}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("cannot write header for %q: %s", e.name, err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func snapshotEntries(prefix string) []entry {
	return []entry{
		dirEntry(prefix + "/"),
		dirEntry(prefix + "/indexdb/"),
		fileEntry(prefix+"/indexdb/parts.json", `["p1"]`),
		dirEntry(prefix + "/indexdb/p1/"),
		fileEntry(prefix+"/indexdb/p1/items.bin", "items"),
		dirEntry(prefix + "/datadb/"),
		fileEntry(prefix+"/datadb/parts.json", `["p2"]`),
		fileEntry(prefix+"/datadb/p2/timestamps.bin", "timestamps"),
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("cannot read %q: %s", p, err)
	}
	return string(b)
}

func TestExtractSnapshotLayout(t *testing.T) {
	for _, prefix := range []string{"./20261104091211-18C876EFEB03599B", "20261104091211-18C876EFEB03599B"} {
		dir := t.TempDir()
		entries := append([]entry{dirEntry("./")}, snapshotEntries(prefix)...)
		st, err := Extract(context.Background(), bytes.NewReader(makeTar(t, entries...)), dir)
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if st.Files != 4 || st.Dirs != 4 || st.DataBytes != int64(len(`["p1"]`)*2+len("items")+len("timestamps")) {
			t.Fatalf("unexpected stats: %+v", st)
		}
		root, err := FindDataRoot(dir)
		if err != nil {
			t.Fatalf("FindDataRoot: %s", err)
		}
		if want := filepath.Join(dir, "20261104091211-18C876EFEB03599B"); root != want {
			t.Fatalf("unexpected data root %q; want %q", root, want)
		}
		if got := readFile(t, filepath.Join(root, "datadb", "p2", "timestamps.bin")); got != "timestamps" {
			t.Fatalf("unexpected file contents %q", got)
		}
		fi, err := os.Stat(filepath.Join(root, "indexdb", "p1", "items.bin"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o644 {
			t.Fatalf("unexpected file mode %s", fi.Mode())
		}
	}
}

func TestExtractRootLayout(t *testing.T) {
	dir := t.TempDir()
	entries := []entry{
		dirEntry("./"),
		dirEntry("./indexdb/"),
		fileEntry("./indexdb/parts.json", "[]"),
		dirEntry("./datadb/"),
		fileEntry("./datadb/parts.json", "[]"),
	}
	if _, err := Extract(context.Background(), bytes.NewReader(makeTar(t, entries...)), dir); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	root, err := FindDataRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if root != dir {
		t.Fatalf("unexpected data root %q; want %q", root, dir)
	}
}

func TestExtractRejectsPathTraversal(t *testing.T) {
	for _, name := range []string{
		"../evil",
		"./../evil",
		"snap/../../evil",
		"/etc/evil",
		"..",
	} {
		parent := t.TempDir()
		dir := filepath.Join(parent, "target")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		data := makeTar(t, fileEntry("snap/indexdb/ok", "ok"), fileEntry(name, "evil"))
		_, err := Extract(context.Background(), bytes.NewReader(data), dir)
		if err == nil {
			t.Fatalf("expecting error for entry %q", name)
		}
		if _, err := os.Stat(filepath.Join(parent, "evil")); err == nil {
			t.Fatalf("entry %q escaped the target directory", name)
		}
	}
}

func TestExtractRejectsHardLinkTraversal(t *testing.T) {
	dir := t.TempDir()
	data := makeTar(t, entry{name: "snap/link", typ: tar.TypeLink, linkname: "../../etc/passwd"})
	if _, err := Extract(context.Background(), bytes.NewReader(data), dir); err == nil {
		t.Fatalf("expecting error for hard link escaping the target directory")
	}
}

func TestExtractSkipsSymlinksAndDevices(t *testing.T) {
	dir := t.TempDir()
	data := makeTar(t,
		fileEntry("snap/indexdb/a", "a"),
		entry{name: "snap/indexdb/sym", typ: tar.TypeSymlink, linkname: "/etc/passwd"},
		entry{name: "snap/indexdb/dev", typ: tar.TypeChar},
		entry{name: "snap/indexdb/fifo", typ: tar.TypeFifo},
	)
	st, err := Extract(context.Background(), bytes.NewReader(data), dir)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if st.Skipped != 3 || st.Files != 1 {
		t.Fatalf("unexpected stats: %+v", st)
	}
	for _, name := range []string{"sym", "dev", "fifo"} {
		if _, err := os.Lstat(filepath.Join(dir, "snap", "indexdb", name)); err == nil {
			t.Fatalf("%s must be skipped", name)
		}
	}
}

func TestExtractHardLink(t *testing.T) {
	dir := t.TempDir()
	data := makeTar(t,
		fileEntry("snap/indexdb/a", "content"),
		entry{name: "snap/datadb/b", typ: tar.TypeLink, linkname: "snap/indexdb/a"},
	)
	st, err := Extract(context.Background(), bytes.NewReader(data), dir)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if st.HardLinks != 1 {
		t.Fatalf("unexpected stats: %+v", st)
	}
	if got := readFile(t, filepath.Join(dir, "snap", "datadb", "b")); got != "content" {
		t.Fatalf("unexpected contents %q", got)
	}
}

func TestExtractDuplicateEntryDoesNotWriteThroughHardLink(t *testing.T) {
	dir := t.TempDir()
	data := makeTar(t,
		fileEntry("snap/a", "original"),
		entry{name: "snap/b", typ: tar.TypeLink, linkname: "snap/a"},
		fileEntry("snap/b", "replaced"),
	)
	if _, err := Extract(context.Background(), bytes.NewReader(data), dir); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if got := readFile(t, filepath.Join(dir, "snap", "a")); got != "original" {
		t.Fatalf("hard link target was modified: %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "snap", "b")); got != "replaced" {
		t.Fatalf("unexpected contents %q", got)
	}
}

func TestExtractTruncatedArchive(t *testing.T) {
	data := makeTar(t, snapshotEntries("snap")...)
	// Cut the archive in the middle of a file body.
	idx := bytes.Index(data, []byte("items"))
	if idx < 0 {
		t.Fatal("cannot find file body")
	}
	_, err := Extract(context.Background(), bytes.NewReader(data[:idx+2]), t.TempDir())
	if err == nil {
		t.Fatalf("expecting error for truncated archive")
	}
}

func TestExtractCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Extract(ctx, bytes.NewReader(makeTar(t, snapshotEntries("snap")...)), t.TempDir())
	if err == nil {
		t.Fatalf("expecting error for canceled context")
	}
}

func TestFindDataRootIgnoresJunk(t *testing.T) {
	dir := t.TempDir()
	entries := append(snapshotEntries("./snap"), fileEntry("./._snap", "apple double"), dirEntry("./other/"), fileEntry("./other/x", "x"))
	if _, err := Extract(context.Background(), bytes.NewReader(makeTar(t, entries...)), dir); err != nil {
		t.Fatal(err)
	}
	root, err := FindDataRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.Join(dir, "snap") {
		t.Fatalf("unexpected root %q", root)
	}
}

func TestFindDataRootErrors(t *testing.T) {
	f := func(errSubstr string, entries ...entry) {
		t.Helper()
		dir := t.TempDir()
		if _, err := Extract(context.Background(), bytes.NewReader(makeTar(t, entries...)), dir); err != nil {
			t.Fatal(err)
		}
		_, err := FindDataRoot(dir)
		if err == nil {
			t.Fatalf("expecting error")
		}
		if !strings.Contains(err.Error(), errSubstr) {
			t.Fatalf("unexpected error %q; want substring %q", err, errSubstr)
		}
	}
	// Empty archive.
	f("no directory")
	// No partition data.
	f("no directory", fileEntry("snap/readme", "x"))
	// Two candidate roots.
	f("expecting exactly one", append(snapshotEntries("a"), snapshotEntries("b")...)...)
	// datadb without indexdb.
	f("must contain both", fileEntry("snap/datadb/parts.json", "[]"))
	// indexdb without datadb at the root.
	f("must contain both", fileEntry("indexdb/parts.json", "[]"))
}

func TestSanitizeName(t *testing.T) {
	ok := map[string]string{
		"./":                  "",
		".":                   "",
		"a":                   "a",
		"./a/b/":              "a/b",
		"a//b":                "a/b",
		"a/./b":               "a/b",
		"a/x/../b":            "a/b",
		"snap/indexdb/..data": "snap/indexdb/..data",
	}
	for in, want := range ok {
		got, err := SanitizeName(in)
		if err != nil {
			t.Fatalf("SanitizeName(%q): unexpected error: %s", in, err)
		}
		if got != want {
			t.Fatalf("SanitizeName(%q): got %q; want %q", in, got, want)
		}
	}
	for _, in := range []string{"", "/", "/a", "..", "../a", "a/../../b", "a\x00b"} {
		if got, err := SanitizeName(in); err == nil {
			t.Fatalf("SanitizeName(%q): expecting error; got %q", in, got)
		}
	}
}

func TestSyncDirs(t *testing.T) {
	dir := t.TempDir()
	if _, err := Extract(context.Background(), bytes.NewReader(makeTar(t, snapshotEntries("snap")...)), dir); err != nil {
		t.Fatal(err)
	}
	if err := SyncDirs(dir); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := SyncDir(filepath.Join(dir, "missing")); err == nil {
		t.Fatalf("expecting error for a missing directory")
	}
}
