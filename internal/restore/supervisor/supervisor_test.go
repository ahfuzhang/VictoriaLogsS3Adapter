package supervisor

import (
	"slices"
	"testing"
)

func TestArgs(t *testing.T) {
	s := New(Config{Bin: "/bin/victoria-logs", StorageDir: "/data"})
	want := []string{
		"-storageDataPath=/data",
		"-httpListenAddr=127.0.0.1:1234",
		"-retentionPeriod=100y",
		"-loggerFormat=json",
	}
	if got := s.Args("127.0.0.1:1234"); !slices.Equal(got, want) {
		t.Fatalf("unexpected args\ngot  %q\nwant %q", got, want)
	}

	s = New(Config{Bin: "/bin/victoria-logs", StorageDir: "/data", AuthKey: "key"})
	got := s.Args("127.0.0.1:1234")
	if got[len(got)-1] != "-partitionManageAuthKey=key" {
		t.Fatalf("auth key must be passed to the child; got %q", got)
	}
	if red := redactArgs(got); red[len(red)-1] != "-partitionManageAuthKey=secret" || got[len(got)-1] != "-partitionManageAuthKey=key" {
		t.Fatalf("unexpected redaction: %q (original %q)", red, got)
	}
}

func TestFilterEnv(t *testing.T) {
	env := []string{"PATH=/bin", "access_key_id=x", "secret_access_key=y", "HOME=/root", "access_key_id_suffix=z"}
	got := filterEnv(env, []string{"access_key_id", "secret_access_key"})
	want := []string{"PATH=/bin", "HOME=/root", "access_key_id_suffix=z"}
	if !slices.Equal(got, want) {
		t.Fatalf("unexpected env\ngot  %q\nwant %q", got, want)
	}
	if got := filterEnv(env, nil); !slices.Equal(got, env) {
		t.Fatalf("env must not change without drop list")
	}
}

func TestFreePort(t *testing.T) {
	port, err := freePort("127.0.0.1")
	if err != nil {
		t.Skipf("cannot listen on loopback: %s", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("unexpected port %d", port)
	}
}
