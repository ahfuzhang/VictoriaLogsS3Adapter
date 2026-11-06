package backup

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseHHMM(t *testing.T) {
	good := map[string][2]int{
		"0000": {0, 0},
		"0015": {0, 15},
		"2359": {23, 59},
		"1200": {12, 0},
	}
	for s, want := range good {
		h, m, err := ParseHHMM(s)
		if err != nil {
			t.Errorf("ParseHHMM(%q): unexpected error: %s", s, err)
			continue
		}
		if h != want[0] || m != want[1] {
			t.Errorf("ParseHHMM(%q) = %02d:%02d; want %02d:%02d", s, h, m, want[0], want[1])
		}
	}
	for _, s := range []string{"", "015", "00150", "2400", "0060", "12:00", "ab12", "-015", " 015", "１２００"} {
		if _, _, err := ParseHHMM(s); err == nil {
			t.Errorf("ParseHHMM(%q): expected an error", s)
		}
	}
}

func TestParseDate(t *testing.T) {
	d, err := ParseDate("20261104")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if got := d.Format(DateLayout); got != "20261104" || d.Location().String() != "UTC" {
		t.Fatalf("unexpected date %s", d)
	}
	if _, err := ParseDate("20240229"); err != nil {
		t.Fatalf("leap day: unexpected error: %s", err)
	}
	for _, s := range []string{"", "2026-11-04", "2026114", "202611040", "20261332", "20261131", "20230229", "2026110a"} {
		if _, err := ParseDate(s); err == nil {
			t.Errorf("ParseDate(%q): expected an error", s)
		}
	}
}

func parseConfig(t *testing.T, args ...string) (*Config, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg := RegisterFlags(fs)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return cfg, cfg.Validate()
}

func TestConfigValidate(t *testing.T) {
	t.Setenv("BACKUP_TEST_NS", "ns1")
	base := []string{
		"-s3.endpoint=http://127.0.0.1:9000",
		"-s3.bucket=b",
		"-victoria.logs.addr=http://127.0.0.1:9428",
		"-victoria.logs.storage.dir=/data",
	}
	with := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }

	cfg, err := parseConfig(t, with("-backup.date=20261104")...)
	if err != nil {
		t.Fatalf("valid one-shot config: unexpected error: %s", err)
	}
	if cfg.MaxUploadBytesPerSecond.N != 100*1024*1024 {
		t.Fatalf("default upload limit = %d; want 100mb", cfg.MaxUploadBytesPerSecond.N)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("default log level = %q; want info", cfg.LogLevel)
	}

	cfg, err = parseConfig(t, with("-backup.yesterday.at=0015", "-max.upload.bytes.per.second=1MB",
		"-s3.prefix=victoria_logs/{BACKUP_TEST_NS}/${YYYYMMDD_HHMISS}")...)
	if err != nil {
		t.Fatalf("valid scheduled config: unexpected error: %s", err)
	}
	if cfg.MaxUploadBytesPerSecond.N != 1024*1024 {
		t.Fatalf("upload limit = %d; want 1mb", cfg.MaxUploadBytesPerSecond.N)
	}

	bad := map[string][]string{
		"no mode":           base,
		"both modes":        with("-backup.date=20261104", "-backup.yesterday.at=0015"),
		"bad date":          with("-backup.date=2026-11-04"),
		"bad hhmm":          with("-backup.yesterday.at=25:00"),
		"bad hour":          with("-backup.yesterday.at=2400"),
		"no endpoint":       {"-s3.bucket=b", "-victoria.logs.addr=x", "-victoria.logs.storage.dir=/d", "-backup.date=20261104"},
		"no bucket":         {"-s3.endpoint=x", "-victoria.logs.addr=x", "-victoria.logs.storage.dir=/d", "-backup.date=20261104"},
		"no vl addr":        {"-s3.endpoint=x", "-s3.bucket=b", "-victoria.logs.storage.dir=/d", "-backup.date=20261104"},
		"no storage dir":    {"-s3.endpoint=x", "-s3.bucket=b", "-victoria.logs.addr=x", "-backup.date=20261104"},
		"unset env in tmpl": with("-backup.date=20261104", "-s3.prefix=a/{BACKUP_TEST_UNSET_VAR}"),
		"bad time var":      with("-backup.date=20261104", "-s3.prefix=a/${YYYYMMDD_HHMMSS}"),
		"unknown time var":  with("-backup.date=20261104", "-s3.prefix=a/${namespace}"),
	}
	for name, args := range bad {
		if _, err := parseConfig(t, args...); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}

	for _, v := range []string{"0", "-1mb", "fast", "10tb"} {
		if _, err := parseConfig(t, with("-backup.date=20261104", "-max.upload.bytes.per.second="+v)...); err == nil {
			t.Errorf("-max.upload.bytes.per.second=%s: expected an error", v)
		}
	}
}

func TestCheckStorageDir(t *testing.T) {
	dir := t.TempDir()
	if err := checkStorageDir(dir); err != nil {
		t.Fatalf("existing dir: unexpected error: %s", err)
	}
	if err := checkStorageDir(filepath.Join(dir, "missing")); err == nil {
		t.Fatalf("missing dir: expected an error")
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkStorageDir(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("regular file: expected a 'not a directory' error; got %v", err)
	}
}
