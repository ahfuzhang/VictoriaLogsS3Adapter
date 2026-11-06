package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

type tarEntry struct {
	name    string
	typ     byte
	mode    os.FileMode
	size    int64
	content string
	modTime time.Time
}

func readTar(t *testing.T, data []byte) []tarEntry {
	t.Helper()
	var out []tarEntry
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("cannot read tar stream: %s", err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("cannot read tar entry %q: %s", hdr.Name, err)
		}
		out = append(out, tarEntry{
			name:    hdr.Name,
			typ:     hdr.Typeflag,
			mode:    hdr.FileInfo().Mode().Perm(),
			size:    hdr.Size,
			content: string(b),
			modTime: hdr.ModTime,
		})
	}
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // not affected by umask
		t.Fatal(err)
	}
}

func TestWriteTar(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "20261105094918-18E51D0B85C98DAF")
	writeFile(t, filepath.Join(src, "datadb", "parts.json"), `["18E51D0E6128EB23"]`, 0o644)
	writeFile(t, filepath.Join(src, "datadb", "18E51D0E6128EB23", "timestamps.bin"), "0123456789", 0o600)
	writeFile(t, filepath.Join(src, "datadb", "18E51D0E6128EB23", "empty.bin"), "", 0o644)
	writeFile(t, filepath.Join(src, "indexdb", "parts.json"), `[]`, 0o640)
	if err := os.MkdirAll(filepath.Join(src, "indexdb", "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("parts.json", filepath.Join(src, "indexdb", "link")); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 11, 4, 10, 20, 30, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(src, "datadb", "parts.json"), mtime, mtime); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	st, err := writeTar(context.Background(), &buf, src, "snap", discardLogger)
	if err != nil {
		t.Fatalf("writeTar: %s", err)
	}
	if st.Files != 4 || st.Dirs != 5 || st.Skipped != 1 || st.DataBytes != int64(len(`["18E51D0E6128EB23"]`)+10+2) {
		t.Fatalf("unexpected stats: %+v", st)
	}

	entries := readTar(t, buf.Bytes())
	var names []string
	byName := map[string]tarEntry{}
	for _, e := range entries {
		names = append(names, e.name)
		byName[e.name] = e
	}
	wantNames := []string{
		"snap/",
		"snap/datadb/",
		"snap/datadb/18E51D0E6128EB23/",
		"snap/datadb/18E51D0E6128EB23/empty.bin",
		"snap/datadb/18E51D0E6128EB23/timestamps.bin",
		"snap/datadb/parts.json",
		"snap/indexdb/",
		"snap/indexdb/emptydir/",
		"snap/indexdb/parts.json",
	}
	if !slices.Equal(names, wantNames) {
		t.Fatalf("tar entries:\n%q\nwant:\n%q", names, wantNames)
	}
	for _, n := range wantNames {
		e := byName[n]
		isDir := n[len(n)-1] == '/'
		if isDir && e.typ != tar.TypeDir || !isDir && e.typ != tar.TypeReg {
			t.Errorf("%s: unexpected type %q", n, e.typ)
		}
	}
	if e := byName["snap/datadb/18E51D0E6128EB23/timestamps.bin"]; e.content != "0123456789" || e.size != 10 || e.mode != 0o600 {
		t.Errorf("timestamps.bin: unexpected entry %+v", e)
	}
	if e := byName["snap/indexdb/parts.json"]; e.content != "[]" || e.mode != 0o640 {
		t.Errorf("indexdb/parts.json: unexpected entry %+v", e)
	}
	if e := byName["snap/datadb/parts.json"]; !e.modTime.Equal(mtime) {
		t.Errorf("datadb/parts.json: mtime %s; want %s", e.modTime, mtime)
	}
	if e := byName["snap/datadb/18E51D0E6128EB23/empty.bin"]; e.size != 0 || e.content != "" {
		t.Errorf("empty.bin: unexpected entry %+v", e)
	}
}

type failingWriter struct {
	n     int
	limit int
}

var errWriterFailed = errors.New("writer failed")

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.n+len(p) > w.limit {
		return 0, errWriterFailed
	}
	w.n += len(p)
	return len(p), nil
}

func TestWriteTarErrors(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a", "big.bin"), string(make([]byte, 64*1024)), 0o644)

	// Writer errors are propagated.
	_, err := writeTar(context.Background(), &failingWriter{limit: 4096}, src, "s", discardLogger)
	if !errors.Is(err, errWriterFailed) {
		t.Fatalf("expected errWriterFailed; got %v", err)
	}

	// A canceled context stops the walk.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = writeTar(ctx, io.Discard, src, "s", discardLogger)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled; got %v", err)
	}

	// A missing source directory is an error.
	_, err = writeTar(context.Background(), io.Discard, filepath.Join(src, "missing"), "s", discardLogger)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist; got %v", err)
	}
}
