package bytesize

import "testing"

func TestParse(t *testing.T) {
	ok := map[string]int64{
		"100mb":  100 * MB,
		"100MB":  100 * MB,
		"100Mb":  100 * MB,
		"1gb":    GB,
		"512kb":  512 * KB,
		"10b":    10,
		"10B":    10,
		"4096":   4096,
		" 2mb ":  2 * MB,
		"1.5mb":  MB + MB/2,
		"100 mb": 100 * MB,
	}
	for in, want := range ok {
		got, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): unexpected error: %s", in, err)
		}
		if got != want {
			t.Fatalf("Parse(%q) = %d; want %d", in, got, want)
		}
	}
	for _, in := range []string{"", "mb", "abc", "-1mb", "0", "10tb", "1.2.3kb"} {
		if _, err := Parse(in); err == nil {
			t.Fatalf("Parse(%q): expected error", in)
		}
	}
}

func TestFlag(t *testing.T) {
	f := NewFlag("100mb")
	if f.N != 100*MB || f.String() != "100mb" {
		t.Fatalf("unexpected default: %d %q", f.N, f.String())
	}
	if err := f.Set("1GB"); err != nil {
		t.Fatal(err)
	}
	if f.N != GB {
		t.Fatalf("got %d; want %d", f.N, GB)
	}
	if err := f.Set("bad"); err == nil {
		t.Fatal("expected error")
	}
}
