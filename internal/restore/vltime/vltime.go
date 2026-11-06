// Package vltime parses the start/end query args of VictoriaLogs select APIs
// exactly like VictoriaLogs does and maps time ranges to per-day partition names.
//
// The parsing code is a port of ParseTimeAt from
// github.com/VictoriaMetrics/VictoriaMetrics/lib/timeutil and of DurationValue
// from github.com/VictoriaMetrics/metricsql, which are used by VictoriaLogs for
// the start, end and time query args.
package vltime

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	// time.UnixNano can only store maxInt64, which is 2262
	maxValidYear = 2262
	minValidYear = 1970
)

// LocalTimezoneOffsetNsecs returns the current offset of the local timezone
// east of UTC in nanoseconds. VictoriaLogs uses the same value for timestamps
// without timezone information.
func LocalTimezoneOffsetNsecs() int64 {
	_, offset := time.Now().Zone()
	return int64(offset) * 1e9
}

// ParseTimeAt parses s in the formats supported by VictoriaLogs and returns
// a unix timestamp in nanoseconds.
//
// Supported formats:
//
//   - now
//   - relative durations such as 5m, 1h30m, -1d, now-1h; the result is currentTimestamp minus the duration
//   - unix timestamps in seconds, milliseconds, microseconds or nanoseconds (integer, fractional or scientific)
//   - YYYY, YYYY-MM, YYYY-MM-DD, YYYY-MM-DDTHH, YYYY-MM-DDTHH:MM, YYYY-MM-DDTHH:MM:SS
//     with optional Z or +HH:MM / -HH:MM suffix
//   - RFC3339 with optional fractional seconds
//
// Timestamps without timezone information are interpreted in the timezone with the
// given localOffsetNsecs (offset east of UTC), exactly like VictoriaLogs does with
// its local timezone. Note that, like in VictoriaLogs, a bare YYYY is always UTC.
func ParseTimeAt(s string, currentTimestamp, localOffsetNsecs int64) (int64, error) {
	if s == "now" {
		return currentTimestamp, nil
	}
	sOrig := s
	tzOffset := int64(0)
	if len(sOrig) > 6 {
		// Try parsing timezone offset
		tz := sOrig[len(sOrig)-6:]
		if (tz[0] == '-' || tz[0] == '+') && tz[3] == ':' {
			isPlus := tz[0] == '+'
			hour, err := strconv.ParseUint(tz[1:3], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("cannot parse hour from timezone offset %q: %w", tz, err)
			}
			minute, err := strconv.ParseUint(tz[4:], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("cannot parse minute from timezone offset %q: %w", tz, err)
			}
			tzOffset = int64(hour*3600+minute*60) * 1e9
			if isPlus {
				tzOffset = -tzOffset
			}
			s = sOrig[:len(sOrig)-6]
		} else {
			if !strings.HasSuffix(s, "Z") {
				tzOffset = -localOffsetNsecs
			} else {
				s = s[:len(s)-1]
			}
		}
	}
	s = strings.TrimSuffix(s, "Z")
	if len(s) > 0 && (s[len(s)-1] > '9' || s[0] == '-') || strings.HasPrefix(s, "now") {
		// Parse duration relative to the current time
		s = strings.TrimPrefix(s, "now")
		d, err := ParseDuration(s)
		if err != nil {
			return 0, err
		}
		if d > 0 {
			d = -d
		}
		return currentTimestamp + int64(d), nil
	}
	if len(s) == 4 {
		// Parse YYYY
		t, err := time.Parse("2006", s)
		if err != nil {
			return 0, err
		}
		y := t.Year()
		if y > maxValidYear || y < minValidYear {
			return 0, fmt.Errorf("cannot parse year from %q: year must in range [%d, %d]", s, minValidYear, maxValidYear)
		}
		return tzOffset + t.UnixNano(), nil
	}
	if !strings.Contains(sOrig, "-") {
		nsec, ok := TryParseUnixTimestamp(sOrig)
		if !ok {
			return 0, fmt.Errorf("cannot parse numeric timestamp %q", sOrig)
		}
		return nsec, nil
	}
	layout := ""
	switch len(s) {
	case 7:
		layout = "2006-01"
	case 10:
		layout = "2006-01-02"
	case 13:
		layout = "2006-01-02T15"
	case 16:
		layout = "2006-01-02T15:04"
	case 19:
		layout = "2006-01-02T15:04:05"
	}
	if layout != "" {
		t, err := time.Parse(layout, s)
		if err != nil {
			return 0, err
		}
		return tzOffset + t.UnixNano(), nil
	}
	// Parse RFC3339
	t, err := time.Parse(time.RFC3339, sOrig)
	if err != nil {
		return 0, err
	}
	return t.UnixNano(), nil
}

// ParseDuration parses a duration such as 1h30m, 1.5d, -5m or 300 (seconds)
// in the format supported by VictoriaLogs (MetricsQL durations).
func ParseDuration(s string) (time.Duration, error) {
	ms, err := durationValue(s, 0)
	if err != nil {
		return 0, err
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// durationValue returns the duration in milliseconds for the given s and the given step.
//
// Duration in s may be combined, i.e. 2h5m, -2h5m or 2h-5m.
func durationValue(s string, step int64) (int64, error) {
	if len(s) == 0 {
		return 0, errors.New("duration cannot be empty")
	}
	lastChar := s[len(s)-1]
	if lastChar >= '0' && lastChar <= '9' || lastChar == '.' {
		// Try parsing floating-point duration
		d, err := strconv.ParseFloat(s, 64)
		if err == nil {
			// Convert the duration to milliseconds.
			return int64(d * 1000), nil
		}
	}
	isMinus := false
	d := float64(0)
	for len(s) > 0 {
		n := scanSingleDuration(s, true)
		if n <= 0 {
			return 0, fmt.Errorf("cannot parse duration %q", s)
		}
		ds := s[:n]
		s = s[n:]
		dLocal, err := parseSingleDuration(ds, step)
		if err != nil {
			return 0, err
		}
		if isMinus && dLocal > 0 {
			dLocal = -dLocal
		}
		d += dLocal
		if dLocal < 0 {
			isMinus = true
		}
	}
	if d > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	if d < math.MinInt64 {
		return math.MinInt64, nil
	}
	return int64(d), nil
}

func parseSingleDuration(s string, step int64) (float64, error) {
	if s == "$__interval" {
		return float64(step), nil
	}

	s = strings.ToLower(s)
	numPart := s[:len(s)-1]
	// Strip trailing m if the duration is in ms
	numPart = strings.TrimSuffix(numPart, "m")
	f, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, fmt.Errorf("cannot parse duration %q: %s", s, err)
	}
	var mp float64
	switch s[len(numPart):] {
	case "ms":
		mp = 1
	case "s":
		mp = 1000
	case "m":
		mp = 60 * 1000
	case "h":
		mp = 60 * 60 * 1000
	case "d":
		mp = 24 * 60 * 60 * 1000
	case "w":
		mp = 7 * 24 * 60 * 60 * 1000
	case "y":
		mp = 365 * 24 * 60 * 60 * 1000
	case "i":
		mp = float64(step)
	default:
		return 0, fmt.Errorf("invalid duration suffix in %q", s)
	}
	return mp * f, nil
}

func scanSingleDuration(s string, canBeNegative bool) int {
	if len(s) == 0 {
		return -1
	}
	i := 0
	if s[0] == '-' && canBeNegative {
		i++
	}
	if s[i:] == "$__interval" {
		return i + len("$__interval")
	}
	for i < len(s) && isDecimalChar(s[i]) {
		i++
	}
	if i == 0 || i == len(s) {
		return -1
	}
	if s[i] == '.' {
		j := i
		i++
		for i < len(s) && isDecimalChar(s[i]) {
			i++
		}
		if i == j || i == len(s) {
			return -1
		}
	}
	switch s[i] {
	case 'm', 'M':
		if i+1 < len(s) {
			switch s[i+1] {
			case 's', 'S':
				// duration in ms
				return i + 2
			case 'i', 'I', 'b', 'B':
				// This is not a duration, but Mi or MB suffix.
				return -1
			}
		}
		// Allow small m for duration in minutes; big M means 1e6.
		if s[i] == 'm' {
			return i + 1
		}
		return -1
	case 's', 'S', 'h', 'H', 'd', 'D', 'w', 'W', 'y', 'Y', 'i', 'I':
		return i + 1
	default:
		return -1
	}
}

func isDecimalChar(ch byte) bool {
	return ch >= '0' && ch <= '9'
}

// TryParseUnixTimestamp parses s as unix timestamp in seconds, milliseconds,
// microseconds or nanoseconds and returns the parsed timestamp in nanoseconds.
//
// The supported formats for s:
//
//   - Integer. For example, 1234567890
//   - Fractional. For example, 1234567890.123
//   - Scientific. For example, 1.23456789e9
func TryParseUnixTimestamp(s string) (int64, bool) {
	if expIdx := getExpIndex(s); expIdx >= 0 {
		// The timestamp is a scientific number such as 1.234e5
		decimalExp, ok := tryParseInt64(s[expIdx+1:])
		if !ok {
			return 0, false
		}
		if decimalExp > math.MaxInt32 || decimalExp < math.MinInt32 {
			return 0, false
		}
		n, ok := tryParseScientificNumberForUnixTimestamp(s[:expIdx], int(decimalExp))
		if !ok {
			return 0, false
		}
		return getUnixTimestampNanoseconds(n), true
	}

	dotIdx := strings.IndexByte(s, '.')
	if dotIdx < 0 {
		// The timestamp is integer.
		n, ok := tryParseInt64(s)
		if !ok {
			return 0, false
		}
		return getUnixTimestampNanoseconds(n), true
	}

	// The timestamp is fractional.
	intStr := s[:dotIdx]
	fracStr := s[dotIdx+1:]
	n, ok := tryParseFractionalNumberForUnixTimestamp(intStr, fracStr)
	if !ok {
		return 0, false
	}

	// Adjust the n to multiples of thousands, since this is expected by getUnixTimestampNanoseconds.
	decimalExp := len(fracStr)
	for decimalExp%3 != 0 {
		if n >= 0 && n > math.MaxInt64/10 || n < 0 && n < math.MinInt64/10 {
			return 0, false
		}
		n *= 10
		decimalExp++
	}

	return getUnixTimestampNanoseconds(n), true
}

func getExpIndex(s string) int {
	if n := strings.IndexByte(s, 'e'); n >= 0 {
		return n
	}
	if n := strings.IndexByte(s, 'E'); n >= 0 {
		return n
	}
	return -1
}

func tryParseScientificNumberForUnixTimestamp(s string, decimalExp int) (int64, bool) {
	dotIdx := strings.IndexByte(s, '.')
	if dotIdx < 0 {
		n, ok := tryParseInt64(s)
		if !ok {
			return 0, false
		}
		return multiplyByDecimalExp(n, decimalExp)
	}

	intStr := s[:dotIdx]
	fracStr := s[dotIdx+1:]
	if decimalExp < len(fracStr) {
		return 0, false
	}
	n, ok := tryParseFractionalNumberForUnixTimestamp(intStr, fracStr)
	if !ok {
		return 0, false
	}
	decimalExp -= len(fracStr)
	return multiplyByDecimalExp(n, decimalExp)
}

func tryParseFractionalNumberForUnixTimestamp(intStr, fracStr string) (int64, bool) {
	n, ok := tryParseInt64(intStr)
	if !ok {
		return 0, false
	}

	decimalExp := len(fracStr)
	num, ok := multiplyByDecimalExp(n, decimalExp)
	if !ok {
		return 0, false
	}

	frac, ok := tryParseInt64(fracStr)
	if !ok {
		return 0, false
	}

	if num >= 0 {
		if num > math.MaxInt64-frac {
			return 0, false
		}
		num += frac
	} else {
		if num < math.MinInt64+frac {
			return 0, false
		}
		num -= frac
	}

	return num, true
}

func multiplyByDecimalExp(n int64, decimalExp int) (int64, bool) {
	if decimalExp < 0 {
		return 0, false
	}
	if decimalExp >= len(decimalMultipliers) {
		return 0, false
	}
	if decimalExp == 0 {
		return n, true
	}

	m := decimalMultipliers[decimalExp]

	if n >= 0 && n > math.MaxInt64/m || n < 0 && n < math.MinInt64/m {
		return 0, false
	}

	return n * m, true
}

var decimalMultipliers = [...]int64{0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10, 1e11, 1e12, 1e13, 1e14, 1e15, 1e16, 1e17, 1e18}

func getUnixTimestampNanoseconds(n int64) int64 {
	if n < (1<<31) && n >= (-1<<31) {
		// The timestamp is in seconds.
		return n * 1e9
	}
	if n < 1e3*(1<<31) && n >= 1e3*(-1<<31) {
		// The timestamp is in milliseconds.
		return n * 1e6
	}
	if n < 1e6*(1<<31) && n >= 1e6*(-1<<31) {
		// The timestamp is in microseconds.
		return n * 1e3
	}
	// The timestamp is in nanoseconds
	return n
}

func tryParseInt64(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
