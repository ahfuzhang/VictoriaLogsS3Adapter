// Package pathtmpl expands S3 key prefix templates.
//
// Supported placeholders:
//
//	{VAR}     - replaced with the value of the environment variable VAR
//	${FORMAT} - replaced with the given day and time in UTC formatted according to FORMAT
//
// FORMAT is any combination of the fields below, each used at most once,
// optionally separated by '-', '_' or '.':
//
//	YYYY - year, 4 digits      (taken from day)
//	MM   - month, 01-12        (taken from day)
//	DD   - day of month, 01-31 (taken from day)
//	HH   - hour, 00-23         (taken from now)
//	MI   - minute, 00-59       (taken from now)
//	SS   - second, 00-59       (taken from now)
//
// For example, with day 2026-11-04 and now 2026-11-05 01:10:28 UTC,
// ${YYYYMMDD_HHMISS} expands to 20261104_011028 and ${DD-MM-YYYY_HH_MI}
// expands to 04-11-2026_01_10. Anything else inside ${...} is rejected by Parse.
//
// The result is normalized: leading/trailing slashes are removed and repeated
// slashes are collapsed.
package pathtmpl

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Template is a parsed prefix template.
type Template struct {
	raw   string
	parts []part
}

type partKind int

const (
	kindLiteral partKind = iota
	kindEnv
	kindTime
)

type part struct {
	kind  partKind
	value string      // literal text or env var name
	tf    *timeFormat // time format for kindTime
}

// Parse parses and validates tmpl. Every referenced environment variable must
// be set to a non-empty value and every ${...} must be a valid time format.
func Parse(tmpl string) (*Template, error) {
	t := &Template{raw: tmpl}
	s := tmpl
	for len(s) > 0 {
		i := strings.IndexByte(s, '{')
		if i < 0 {
			t.addLiteral(s)
			break
		}
		isTime := i > 0 && s[i-1] == '$'
		lit := s[:i]
		if isTime {
			lit = s[:i-1]
		}
		t.addLiteral(lit)
		end := strings.IndexByte(s[i:], '}')
		if end < 0 {
			return nil, fmt.Errorf("unclosed '{' in template %q", tmpl)
		}
		name := s[i+1 : i+end]
		s = s[i+end+1:]
		if name == "" {
			return nil, fmt.Errorf("empty placeholder in template %q", tmpl)
		}
		if isTime {
			tf, err := parseTimeFormat(name)
			if err != nil {
				return nil, fmt.Errorf("invalid time placeholder ${%s} in template %q: %w", name, tmpl, err)
			}
			t.parts = append(t.parts, part{kind: kindTime, tf: tf})
			continue
		}
		if v, ok := os.LookupEnv(name); !ok || v == "" {
			return nil, fmt.Errorf("environment variable %q referenced by template %q is not set or empty", name, tmpl)
		}
		t.parts = append(t.parts, part{kind: kindEnv, value: name})
	}
	return t, nil
}

func (t *Template) addLiteral(s string) {
	if s != "" {
		t.parts = append(t.parts, part{kind: kindLiteral, value: s})
	}
}

// String returns the raw template.
func (t *Template) String() string { return t.raw }

// HasTime reports whether the template contains a ${...} time placeholder.
func (t *Template) HasTime() bool {
	for _, p := range t.parts {
		if p.kind == kindTime {
			return true
		}
	}
	return false
}

// Expand expands the template. Inside time placeholders the date fields
// (YYYY, MM, DD) are taken from day and the time-of-day fields (HH, MI, SS)
// from now; both are converted to UTC first.
func (t *Template) Expand(day, now time.Time) string {
	var sb strings.Builder
	for _, p := range t.parts {
		switch p.kind {
		case kindLiteral:
			sb.WriteString(p.value)
		case kindEnv:
			sb.WriteString(os.Getenv(p.value))
		case kindTime:
			sb.WriteString(p.tf.format(day.UTC(), now.UTC()))
		}
	}
	return Normalize(sb.String())
}

// StaticPrefix returns the expansion of the template up to the first time
// placeholder. It is suitable for listing objects produced by Expand with any
// time value. The returned value is not normalized at its end, so it may end
// with a partial path segment (e.g. "logs/backup_"). Leading slashes are removed
// and repeated slashes are collapsed. If the template has no time placeholder,
// the result equals Expand(...) + "/" (or "" when the expansion is empty).
func (t *Template) StaticPrefix() string {
	var sb strings.Builder
	for _, p := range t.parts {
		if p.kind == kindTime {
			return collapse(strings.TrimLeft(sb.String(), "/"))
		}
		if p.kind == kindLiteral {
			sb.WriteString(p.value)
		} else {
			sb.WriteString(os.Getenv(p.value))
		}
	}
	s := Normalize(sb.String())
	if s == "" {
		return ""
	}
	return s + "/"
}

// timeSentinel stands for a time placeholder while building a key regexp.
// It contains no slashes, so normalization treats it like a real time value.
const timeSentinel = "\x00time\x00"

// KeyRegexp returns a regexp matching exactly the keys pathtmpl.Join(t.Expand(anyDay, anyTime), name)
// for any day and time values. It is used to recognize objects produced with this template
// when listing a bucket, without matching objects of other prefixes.
func (t *Template) KeyRegexp(name string) *regexp.Regexp {
	var sb strings.Builder
	var timePatterns []string
	for _, p := range t.parts {
		switch p.kind {
		case kindLiteral:
			sb.WriteString(p.value)
		case kindEnv:
			sb.WriteString(os.Getenv(p.value))
		case kindTime:
			sb.WriteString(timeSentinel)
			timePatterns = append(timePatterns, p.tf.regexpSource())
		}
	}
	// Normalization never removes a sentinel, so the pieces are separated by
	// the time placeholders in template order.
	pieces := strings.Split(Join(sb.String(), name), timeSentinel)
	var re strings.Builder
	re.WriteString(`^`)
	for i, piece := range pieces {
		if i > 0 {
			re.WriteString(timePatterns[i-1])
		}
		re.WriteString(regexp.QuoteMeta(piece))
	}
	re.WriteString(`$`)
	return regexp.MustCompile(re.String())
}

// Normalize trims leading/trailing slashes and collapses repeated slashes.
func Normalize(s string) string {
	return strings.Trim(collapse(s), "/")
}

func collapse(s string) string {
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return s
}

// Join joins a normalized prefix and a name with a slash, handling an empty prefix.
func Join(prefix, name string) string {
	prefix = Normalize(prefix)
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}
