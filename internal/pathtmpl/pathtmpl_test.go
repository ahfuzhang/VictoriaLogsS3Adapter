package pathtmpl

import (
	"strings"
	"testing"
	"time"
)

func TestExpand(t *testing.T) {
	t.Setenv("namespace", "ns1")
	t.Setenv("env", "prod")
	t.Setenv("auth.access_key_id", "dotted")
	now := time.Date(2026, 11, 4, 9, 10, 28, 0, time.FixedZone("UTC+8", 8*3600))

	cases := []struct {
		tmpl       string
		want       string
		hasTime    bool
		staticWant string
	}{
		{"victoria_logs/{namespace}/{env}", "victoria_logs/ns1/prod", false, "victoria_logs/ns1/prod/"},
		{"/victoria_logs//{namespace}/{env}/", "victoria_logs/ns1/prod", false, "victoria_logs/ns1/prod/"},
		{"victoria_logs/{env}/${YYYYMMDD_HHMISS}", "victoria_logs/prod/20261104_011028", true, "victoria_logs/prod/"},
		{"logs/{env}/bk_${YYYYMMDD_HHMISS}/x", "logs/prod/bk_20261104_011028/x", true, "logs/prod/bk_"},
		{"${YYYYMMDD_HHMISS}", "20261104_011028", true, ""},
		{"a/${DD-MM-YYYY_HH_MI}", "a/04-11-2026_01_10", true, "a/"},
		{"x/${SS.MI.HH}", "x/28.10.01", true, "x/"},
		{"${YYYYMMDD}", "20261104", true, ""},
		{"${DD_YYYY}", "04_2026", true, ""},
		{"{env}/${YYYY}/${MM}/${DD}/b", "prod/2026/11/04/b", true, "prod/"},
		{"${HH}h${MI}m", "01h10m", true, ""},
		{"a/${YYYY--MM__DD..HH}", "a/2026--11__04..01", true, "a/"},
		{"{auth.access_key_id}", "dotted", false, "dotted/"},
		{"", "", false, ""},
		{"plain", "plain", false, "plain/"},
	}
	for _, c := range cases {
		tm, err := Parse(c.tmpl)
		if err != nil {
			t.Fatalf("Parse(%q): %s", c.tmpl, err)
		}
		if got := tm.Expand(now); got != c.want {
			t.Fatalf("Expand(%q) = %q; want %q", c.tmpl, got, c.want)
		}
		if tm.HasTime() != c.hasTime {
			t.Fatalf("HasTime(%q) = %v; want %v", c.tmpl, tm.HasTime(), c.hasTime)
		}
		if got := tm.StaticPrefix(); got != c.staticWant {
			t.Fatalf("StaticPrefix(%q) = %q; want %q", c.tmpl, got, c.staticWant)
		}
		if tm.String() != c.tmpl {
			t.Fatalf("String() = %q; want %q", tm.String(), c.tmpl)
		}
	}
}

func TestParseErrors(t *testing.T) {
	t.Setenv("empty_var", "")
	for _, tmpl := range []string{
		"a/{missing_var_xyz}",
		"a/{empty_var}",
		"a/{unclosed",
		"a/{}",
		"a/${}",
		"a/${YYYYMMDD_HHMMSS}", // MM twice: minutes are MI
		"a/${YYYY}${MM}${MM}x/${DDDD}",
		"a/${yyyymmdd}",
		"a/${YYYY-MM-DDTHH}",
		"a/${YYY}",
		"a/${YYYYY}",
		"a/${M}",
		"a/${-_.}",
		"a/${YYYY MM}",
		"a/${YYYY/MM}",
		"a/${DD:MM}",
		"a/${namespace}",
		"a/${YYYY",
	} {
		if _, err := Parse(tmpl); err == nil {
			t.Fatalf("Parse(%q): expected error", tmpl)
		}
	}
}

func TestParseTimeErrorMessages(t *testing.T) {
	cases := map[string]string{
		"a/${YYYYMMDD_HHMMSS}": "MM (month) is used more than once (MM means month; write minutes as MI)",
		"a/${SSSS}":            "SS (second) is used more than once",
		"a/${YYYYMMDDTHH}":     `unrecognized text "THH" at offset 8`,
		"a/${__}":              "no time field found",
	}
	for tmpl, want := range cases {
		_, err := Parse(tmpl)
		if err == nil {
			t.Fatalf("Parse(%q): expected error", tmpl)
		}
		if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), tmpl) {
			t.Fatalf("Parse(%q) error %q must mention %q and the template", tmpl, err, want)
		}
	}
}

func TestJoin(t *testing.T) {
	cases := [][3]string{
		{"", "20261104.tar", "20261104.tar"},
		{"a/b", "20261104.tar", "a/b/20261104.tar"},
		{"/a/b/", "20261104.tar", "a/b/20261104.tar"},
	}
	for _, c := range cases {
		if got := Join(c[0], c[1]); got != c[2] {
			t.Fatalf("Join(%q, %q) = %q; want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestKeyRegexp(t *testing.T) {
	t.Setenv("env", "prod")
	t.Setenv("weird", "a.b+c")
	cases := []struct {
		tmpl    string
		match   []string
		noMatch []string
	}{
		{
			tmpl:    "vl/${YYYYMMDD_HHMISS}/{env}",
			match:   []string{"vl/20261105_001500/prod/20261104.tar"},
			noMatch: []string{"vl/20261105_001500/test/20261104.tar", "vl/20261105_001500/prod/x/20261104.tar", "vl/prod/20261104.tar", "vl/2026110_001500/prod/20261104.tar"},
		},
		{
			tmpl:    "/vl//{env}/bk_${YYYYMMDD_HHMISS}/",
			match:   []string{"vl/prod/bk_20261105_001500/20261104.tar"},
			noMatch: []string{"vl/prod/bk_20261105_001500/20261103.tar", "xvl/prod/bk_20261105_001500/20261104.tar"},
		},
		{
			tmpl:    "${YYYYMMDD_HHMISS}",
			match:   []string{"20261105_001500/20261104.tar"},
			noMatch: []string{"a/20261105_001500/20261104.tar"},
		},
		{
			tmpl:    "{weird}/${YYYYMMDD_HHMISS}",
			match:   []string{"a.b+c/20261105_001500/20261104.tar"},
			noMatch: []string{"aXb+c/20261105_001500/20261104.tar"},
		},
		{
			tmpl:  "vl/${DD-MM-YYYY_HH_MI}/{env}",
			match: []string{"vl/05-11-2026_00_15/prod/20261104.tar"},
			noMatch: []string{
				"vl/20261105_001500/prod/20261104.tar",
				"vl/05-11-2026_00-15/prod/20261104.tar",
				"vl/5-11-2026_00_15/prod/20261104.tar",
			},
		},
		{
			tmpl:  "${YYYY}/${MM}/${DD}/bk.${HHMI}",
			match: []string{"2026/11/05/bk.0015/20261104.tar", "2025/01/31/bk.2359/20261104.tar"},
			noMatch: []string{
				"2026/11/05/bkX0015/20261104.tar",
				"2026/11/bk.0015/20261104.tar",
				"2026/11/05/bk.001500/20261104.tar",
			},
		},
	}
	for _, c := range cases {
		tm, err := Parse(c.tmpl)
		if err != nil {
			t.Fatal(err)
		}
		re := tm.KeyRegexp("20261104.tar")
		// A key produced by Expand must always match.
		generated := Join(tm.Expand(time.Date(2026, 11, 5, 0, 15, 0, 0, time.UTC)), "20261104.tar")
		if !re.MatchString(generated) {
			t.Fatalf("%q: regexp %s does not match generated key %q", c.tmpl, re, generated)
		}
		for _, k := range c.match {
			if !re.MatchString(k) {
				t.Fatalf("%q: regexp %s must match %q", c.tmpl, re, k)
			}
		}
		for _, k := range c.noMatch {
			if re.MatchString(k) {
				t.Fatalf("%q: regexp %s must not match %q", c.tmpl, re, k)
			}
		}
	}
}
