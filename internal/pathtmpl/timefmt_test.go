package pathtmpl

import (
	"testing"
	"time"
)

func TestTimeFormatPadding(t *testing.T) {
	ts := time.Date(987, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := map[string]string{
		"YYYY.MM.DD_HH.MI.SS": "0987.01.02_03.04.05",
		"SSMIHHDDMMYYYY":      "05040302010987",
		"MI":                  "04",
		"-YYYY-":              "-0987-",
	}
	for format, want := range cases {
		f, err := parseTimeFormat(format)
		if err != nil {
			t.Fatalf("parseTimeFormat(%q): %s", format, err)
		}
		if got := f.format(ts, ts); got != want {
			t.Fatalf("format(%q) = %q; want %q", format, got, want)
		}
	}
}
