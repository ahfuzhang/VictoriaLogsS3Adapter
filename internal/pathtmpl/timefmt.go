package pathtmpl

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// timeField is one of the fields that may be used inside a ${...} time placeholder.
type timeField struct {
	token string // the text written by the user, e.g. "YYYY"
	name  string // human readable name used in error messages
	width int    // number of zero-padded digits produced
	// isDate reports whether the field is taken from the day argument of
	// Expand (YYYY, MM, DD) rather than from the now argument (HH, MI, SS).
	isDate bool
	value  func(t time.Time) int
}

// timeFields lists the supported fields. The tokens are prefix-free, so a
// left-to-right scan splits a format into fields unambiguously.
var timeFields = []timeField{
	{"YYYY", "year", 4, true, func(t time.Time) int { return t.Year() }},
	{"MM", "month", 2, true, func(t time.Time) int { return int(t.Month()) }},
	{"DD", "day", 2, true, func(t time.Time) int { return t.Day() }},
	{"HH", "hour", 2, false, func(t time.Time) int { return t.Hour() }},
	{"MI", "minute", 2, false, func(t time.Time) int { return t.Minute() }},
	{"SS", "second", 2, false, func(t time.Time) int { return t.Second() }},
}

// TimeSeparators are the characters allowed between the fields of a time placeholder.
const TimeSeparators = "-_."

// timeFormatHelp describes the time placeholder syntax for error messages.
const timeFormatHelp = "a time placeholder may only contain YYYY (year), MM (month), DD (day), " +
	"HH (hour), MI (minute), SS (second), each at most once, optionally separated by '-', '_' or '.'"

// timeElem is either a field (field >= 0) or a literal separator run.
type timeElem struct {
	field int
	sep   string
}

// timeFormat is a parsed ${...} time placeholder, e.g. ${DD-MM-YYYY_HH_MI}.
type timeFormat struct {
	raw   string // the text between "${" and "}"
	elems []timeElem
}

// parseTimeFormat parses the contents of a ${...} placeholder.
func parseTimeFormat(s string) (*timeFormat, error) {
	f := &timeFormat{raw: s}
	used := make([]bool, len(timeFields))
	hasField := false
	for i := 0; i < len(s); {
		if strings.IndexByte(TimeSeparators, s[i]) >= 0 {
			j := i + 1
			for j < len(s) && strings.IndexByte(TimeSeparators, s[j]) >= 0 {
				j++
			}
			f.elems = append(f.elems, timeElem{field: -1, sep: s[i:j]})
			i = j
			continue
		}
		fi := -1
		for k, tf := range timeFields {
			if strings.HasPrefix(s[i:], tf.token) {
				fi = k
				break
			}
		}
		if fi < 0 {
			return nil, fmt.Errorf("unrecognized text %q at offset %d; %s", s[i:], i, timeFormatHelp)
		}
		tf := timeFields[fi]
		if used[fi] {
			hint := ""
			if tf.token == "MM" {
				hint = " (MM means month; write minutes as MI)"
			}
			return nil, fmt.Errorf("%s (%s) is used more than once%s", tf.token, tf.name, hint)
		}
		used[fi] = true
		hasField = true
		f.elems = append(f.elems, timeElem{field: fi})
		i += len(tf.token)
	}
	if !hasField {
		return nil, fmt.Errorf("no time field found; %s", timeFormatHelp)
	}
	return f, nil
}

// appendTime appends f formatted with the date fields taken from day and the
// time-of-day fields taken from now to dst.
func (f *timeFormat) appendTime(dst []byte, day, now time.Time) []byte {
	for _, e := range f.elems {
		if e.field < 0 {
			dst = append(dst, e.sep...)
			continue
		}
		tf := timeFields[e.field]
		t := now
		if tf.isDate {
			t = day
		}
		v := strconv.Itoa(tf.value(t))
		for n := len(v); n < tf.width; n++ {
			dst = append(dst, '0')
		}
		dst = append(dst, v...)
	}
	return dst
}

// format returns f formatted with the date fields taken from day and the
// time-of-day fields taken from now.
func (f *timeFormat) format(day, now time.Time) string {
	return string(f.appendTime(nil, day, now))
}

// regexpSource returns a regexp source matching any value produced by format.
func (f *timeFormat) regexpSource() string {
	var sb strings.Builder
	for _, e := range f.elems {
		if e.field < 0 {
			sb.WriteString(regexp.QuoteMeta(e.sep))
			continue
		}
		fmt.Fprintf(&sb, "[0-9]{%d}", timeFields[e.field].width)
	}
	return sb.String()
}
