package s3store

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/pathtmpl"
)

func TestBackupKey(t *testing.T) {
	t.Setenv("env", "prod")
	now := time.Date(2026, 11, 5, 0, 15, 0, 0, time.UTC)
	cases := map[string]string{
		"victoria_logs/{env}":                     "victoria_logs/prod/20261104.tar",
		"victoria_logs/{env}/${YYYYMMDD_HHMISS}/": "victoria_logs/prod/20261104_001500/20261104.tar",
		"victoria_logs/{env}/${YYYY}/${MM}/${DD}": "victoria_logs/prod/2026/11/04/20261104.tar",
		"": "20261104.tar",
	}
	for tmpl, want := range cases {
		tm, err := pathtmpl.Parse(tmpl)
		if err != nil {
			t.Fatal(err)
		}
		got, err := BackupKey(tm, now, "20261104")
		if err != nil {
			t.Fatalf("BackupKey(%q): %s", tmpl, err)
		}
		if got != want {
			t.Fatalf("BackupKey(%q) = %q; want %q", tmpl, got, want)
		}
	}
	tm, err := pathtmpl.Parse("${YYYYMMDD}")
	if err != nil {
		t.Fatal(err)
	}
	for _, date := range []string{"", "2026110", "20261332", "2026-11-04"} {
		if _, err := BackupKey(tm, now, date); err == nil {
			t.Fatalf("BackupKey(%q): expected error", date)
		}
	}
}

func mustBackupKey(t *testing.T, tmpl *pathtmpl.Template, now time.Time, date string) string {
	t.Helper()
	key, err := BackupKey(tmpl, now, date)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestCredentialsFromEnv(t *testing.T) {
	t.Setenv(EnvAccessKeyID, "")
	t.Setenv(EnvSecretAccessKey, "")
	if _, _, err := CredentialsFromEnv(); err == nil {
		t.Fatal("expected error for empty credentials")
	}
	t.Setenv(EnvAccessKeyID, "ak")
	t.Setenv(EnvSecretAccessKey, "sk")
	ak, sk, err := CredentialsFromEnv()
	if err != nil || ak != "ak" || sk != "sk" {
		t.Fatalf("unexpected result: %q %q %v", ak, sk, err)
	}
}

// TestIntegration runs against a real S3-compatible endpoint.
// Set S3STORE_TEST_ENDPOINT (e.g. http://127.0.0.1:19000) and S3STORE_TEST_BUCKET.
func TestIntegration(t *testing.T) {
	endpoint := os.Getenv("S3STORE_TEST_ENDPOINT")
	bucket := os.Getenv("S3STORE_TEST_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("S3STORE_TEST_ENDPOINT and S3STORE_TEST_BUCKET are not set")
	}
	t.Setenv(EnvAccessKeyID, "x")
	t.Setenv(EnvSecretAccessKey, "y")
	t.Setenv("s3store_test_env", "it")
	ctx := context.Background()

	if _, err := New(ctx, &Config{Endpoint: endpoint, Bucket: "no-such-bucket-xyz"}); err == nil {
		t.Fatal("expected error for missing bucket")
	}
	c, err := New(ctx, &Config{Endpoint: endpoint, Region: "us-east-1", Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}

	tmpl, err := pathtmpl.Parse("s3store/{s3store_test_env}/${YYYYMMDD_HHMISS}")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 11, 5, 0, 15, 0, 0, time.UTC)
	newer := old.Add(time.Hour)
	data := bytes.Repeat([]byte("0123456789"), 1<<20) // 10MiB, larger than a single small part
	keyOld := mustBackupKey(t, tmpl, old, "20261104")
	keyNew := mustBackupKey(t, tmpl, newer, "20261104")
	for _, k := range []string{keyOld, keyNew} {
		n, err := c.Upload(ctx, k, bytes.NewReader(data), "application/x-tar")
		if err != nil {
			t.Fatal(err)
		}
		if n != int64(len(data)) {
			t.Fatalf("uploaded %d bytes; want %d", n, len(data))
		}
		time.Sleep(1100 * time.Millisecond) // make LastModified differ
	}
	defer func() {
		_ = c.Delete(ctx, keyOld)
		_ = c.Delete(ctx, keyNew)
	}()

	// An object of another prefix sharing the static part must be ignored even if it is newer.
	otherTmpl, err := pathtmpl.Parse("s3store/{s3store_test_env}/${YYYYMMDD_HHMISS}/other")
	if err != nil {
		t.Fatal(err)
	}
	keyOther := mustBackupKey(t, otherTmpl, newer.Add(time.Hour), "20261104")
	if _, err := c.Upload(ctx, keyOther, bytes.NewReader([]byte("other")), ""); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Delete(ctx, keyOther) }()

	info, found, err := c.FindBackup(ctx, tmpl, "20261104")
	if err != nil || !found {
		t.Fatalf("FindBackup: found=%v err=%v", found, err)
	}
	if info.Key != keyNew {
		t.Fatalf("FindBackup returned %q; want newest %q", info.Key, keyNew)
	}
	if _, found, err := c.FindBackup(ctx, tmpl, "20261103"); err != nil || found {
		t.Fatalf("FindBackup for missing date: found=%v err=%v", found, err)
	}

	r, size, err := c.Open(ctx, keyNew)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || size != int64(len(data)) || !bytes.Equal(got, data) {
		t.Fatalf("Open/read mismatch: size=%d len=%d err=%v", size, len(got), err)
	}

	staticTmpl, err := pathtmpl.Parse("s3store/{s3store_test_env}/static")
	if err != nil {
		t.Fatal(err)
	}
	keyStatic := mustBackupKey(t, staticTmpl, time.Now(), "20261102")
	if _, err := c.Upload(ctx, keyStatic, bytes.NewReader([]byte("abc")), ""); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Delete(ctx, keyStatic) }()
	info, found, err = c.FindBackup(ctx, staticTmpl, "20261102")
	if err != nil || !found || info.Key != keyStatic {
		t.Fatalf("FindBackup static: %+v found=%v err=%v", info, found, err)
	}
	if _, found, err := c.Stat(ctx, "s3store/none.tar"); err != nil || found {
		t.Fatalf("Stat missing: found=%v err=%v", found, err)
	}
}

// TestIntegrationMultipart uploads an object larger than DefaultPartSize.
// Set S3STORE_TEST_ENDPOINT, S3STORE_TEST_BUCKET and S3STORE_TEST_LARGE=1.
func TestIntegrationMultipart(t *testing.T) {
	endpoint := os.Getenv("S3STORE_TEST_ENDPOINT")
	bucket := os.Getenv("S3STORE_TEST_BUCKET")
	if endpoint == "" || bucket == "" || os.Getenv("S3STORE_TEST_LARGE") == "" {
		t.Skip("S3STORE_TEST_ENDPOINT, S3STORE_TEST_BUCKET and S3STORE_TEST_LARGE are not set")
	}
	t.Setenv(EnvAccessKeyID, "x")
	t.Setenv(EnvSecretAccessKey, "y")
	ctx := context.Background()
	c, err := New(ctx, &Config{Endpoint: endpoint, Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	size := int64(DefaultPartSize + 5*1024*1024 + 123)
	key := "s3store/multipart.bin"
	pattern := func(i int64) byte { return byte(i*7 + i/4096) }
	pr, pw := io.Pipe()
	go func() {
		buf := make([]byte, 1<<20)
		for off := int64(0); off < size; {
			n := min(int64(len(buf)), size-off)
			for i := int64(0); i < n; i++ {
				buf[i] = pattern(off + i)
			}
			if _, err := pw.Write(buf[:n]); err != nil {
				return
			}
			off += n
		}
		_ = pw.Close()
	}()
	n, err := c.Upload(ctx, key, pr, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Delete(ctx, key) }()
	if n != size {
		t.Fatalf("uploaded %d bytes; want %d", n, size)
	}
	r, gotSize, err := c.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if gotSize != size {
		t.Fatalf("stored size %d; want %d", gotSize, size)
	}
	buf := make([]byte, 1<<20)
	var off int64
	for {
		m, err := r.Read(buf)
		for i := 0; i < m; i++ {
			if buf[i] != pattern(off+int64(i)) {
				t.Fatalf("data mismatch at offset %d", off+int64(i))
			}
		}
		off += int64(m)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if off != size {
		t.Fatalf("read %d bytes; want %d", off, size)
	}
}
