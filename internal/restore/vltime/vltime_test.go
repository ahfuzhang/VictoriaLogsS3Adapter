package vltime

import (
	"slices"
	"strconv"
	"testing"
	"time"
)

// The test cases below are ported from lib/timeutil/time_test.go of VictoriaMetrics,
// which implements the parser used by VictoriaLogs for start/end query args.

func TestTryParseUnixTimestampSuccess(t *testing.T) {
	f := func(s string, want int64) {
		t.Helper()
		got, ok := TryParseUnixTimestamp(s)
		if !ok {
			t.Fatalf("cannot parse timestamp %q", s)
		}
		if got != want {
			t.Fatalf("TryParseUnixTimestamp(%q): got %d; want %d", s, got, want)
		}
	}

	f("0", 0)

	// nanoseconds
	f("-1234567890123456789", -1234567890123456789)
	f("1234567890123456789", 1234567890123456789)
	f("1234567890123456.789", 1234567890123456789)

	// microseconds
	f("-1234567890123456", -1234567890123456000)
	f("1234567890123456", 1234567890123456000)

	// milliseconds
	f("-1234567890123", -1234567890123000000)
	f("1234567890123", 1234567890123000000)
	f("1234567890123.456", 1234567890123456000)

	// seconds
	f("-1234567890", -1234567890000000000)
	f("1234567890", 1234567890000000000)
	f("1234567890.123456789", 1234567890123456789)
	f("1234567890.12345678", 1234567890123456780)
	f("1234567890.1234567", 1234567890123456700)
	f("-1234567890.123456", -1234567890123456000)
	f("-1234567890.1", -1234567890100000000)

	// scientific notation
	f("1e9", 1000000000000000000)
	f("1.234e9", 1234000000000000000)
	f("-1.23456789e9", -1234567890000000000)
	f("1.234567890123456789e18", 1234567890123456789)
	f("0.23456789e9", 234567890000000000)
	f("123.678912e7", 1236789120000000000)
	f("1.23e2", 123000000000)
	f("1123.456789123456789E15", 1123456789123456789)
}

func TestTryParseUnixTimestampFailure(t *testing.T) {
	f := func(s string) {
		t.Helper()
		if _, ok := TryParseUnixTimestamp(s); ok {
			t.Fatalf("expecting failure when parsing %q", s)
		}
	}
	f("")
	f("foobar")
	f("foo.bar")
	f("1.12345671x34")
	f("1233344.")
	f("12345678901234567.891")
	f("12345678901234567890")
	f("0.1234567890123456789123")
	f("1e19")
	f("1.3e123456789090123")
	f("1.23e1")
	f("1E-1")
}

func TestParseTimeAtSuccess(t *testing.T) {
	f := func(s string, currentTime, localOffset, want int64) {
		t.Helper()
		got, err := ParseTimeAt(s, currentTime, localOffset)
		if err != nil {
			t.Fatalf("ParseTimeAt(%q): unexpected error: %s", s, err)
		}
		if got != want {
			t.Fatalf("ParseTimeAt(%q): got %d (%s); want %d (%s)", s, got, time.Unix(0, got).UTC(), want, time.Unix(0, want).UTC())
		}
	}

	now := time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC).UnixNano()

	// unix timestamps
	f("1562529662", now, 0, 1562529662*1e9)
	f("1562529662.678", now, 0, 1562529662678*1e6)
	f("1562529662678", now, 0, 1562529662678*1e6)
	f("1562529662678901", now, 0, 1562529662678901*1e3)
	f("1562529662678901234", now, 0, 1562529662678901234)

	// relative to the current time
	f("now", now, 0, now)
	f("1h5s", now, 0, now-3605*1e9)
	f("5m", now, 0, now-5*60*1e9)
	f("1d", now, 0, now-24*3600*1e9)
	f("1w", now, 0, now-7*24*3600*1e9)
	f("-5m", now, 0, now-5*60*1e9)
	f("-123", now, 0, now-123*1e9)
	f("-123.456", now, 0, now-123456*1e6)
	f("now-1h5m", now, 0, now-(3600+5*60)*1e9)
	f("now-30m", now, 8*3600*1e9, now-30*60*1e9)

	// Year
	f("2023Z", now, 0, 1.6725312e+09*1e9)
	f("2023+02:00", now, 0, 1.672524e+09*1e9)
	f("2023-02:00", now, 0, 1.6725384e+09*1e9)

	// Year and month
	f("2023-05Z", now, 0, 1.6828992e+09*1e9)
	f("2023-05+02:00", now, 0, 1.682892e+09*1e9)
	f("2023-05-02:00", now, 0, 1.6829064e+09*1e9)

	// Year, month and day
	f("2023-05-20Z", now, 0, 1.6845408e+09*1e9)
	f("2023-05-20+02:30", now, 0, 1.6845318e+09*1e9)
	f("2023-05-20-02:30", now, 0, 1.6845498e+09*1e9)

	// Year, month, day and hour
	f("2023-05-20T04Z", now, 0, 1.6845552e+09*1e9)
	f("2023-05-20T04+02:30", now, 0, 1.6845462e+09*1e9)
	f("2023-05-20T04-02:30", now, 0, 1.6845642e+09*1e9)

	// Year, month, day, hour and minute
	f("2023-05-20T04:57Z", now, 0, 1.68455862e+09*1e9)
	f("2023-05-20T04:57+02:30", now, 0, 1.68454962e+09*1e9)
	f("2023-05-20T04:57-02:30", now, 0, 1.68456762e+09*1e9)

	// Year, month, day, hour, minute and second
	f("2023-05-20T04:57:43Z", now, 0, 1.684558663e+09*1e9)
	f("2023-05-20T04:57:43+02:30", now, 0, 1.684549663e+09*1e9)
	f("2023-05-20T04:57:43-02:30", now, 0, 1.684567663e+09*1e9)

	// fractional seconds (RFC3339)
	f("2023-05-20T04:57:43.123Z", now, 0, 1684558663123000000)
	f("2023-05-20T04:57:43.123456789+02:30", now, 0, 1684549663123456789)
	f("2023-05-20T04:57:43.123456789-02:30", now, 0, 1684567663123456789)

	// Timestamps without timezone use the local timezone, like VictoriaLogs does.
	utc8 := int64(8 * 3600 * 1e9)
	f("2026-11-04", now, 0, time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC).UnixNano())
	f("2026-11-04", now, utc8, time.Date(2026, 11, 3, 16, 0, 0, 0, time.UTC).UnixNano())
	f("2026-11-04T10:20:30", now, utc8, time.Date(2026, 11, 4, 2, 20, 30, 0, time.UTC).UnixNano())
	f("2026-11-04T10:20:30Z", now, utc8, time.Date(2026, 11, 4, 10, 20, 30, 0, time.UTC).UnixNano())
	f("2026-11", now, utc8, time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC).UnixNano())
	// A bare year is always UTC in VictoriaLogs, since the timezone detection
	// is performed only for values longer than 6 chars.
	f("2026", now, utc8, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
}

func TestParseTimeAtFailure(t *testing.T) {
	f := func(s string) {
		t.Helper()
		if got, err := ParseTimeAt(s, time.Now().UnixNano(), 0); err == nil {
			t.Fatalf("ParseTimeAt(%q): expecting error; got %d", s, got)
		}
	}
	f("")
	f("2263")
	f("23-45:50")
	f("1223-fo:ba")
	f("1223-12:ba")
	f("23-45")
	f("-123foobar")
	f("2oo5")
	f("2oob-a5")
	f("2oob-ar-a5")
	f("2oob-ar-azTx5")
	f("2oob-ar-azTxx:y5")
	f("2oob-ar-azTxx:yy:z5")
	f("2026-11-04 10:00:00")
	f("yesterday")
}

func TestParseDuration(t *testing.T) {
	f := func(s string, want time.Duration) {
		t.Helper()
		got, err := ParseDuration(s)
		if err != nil {
			t.Fatalf("ParseDuration(%q): unexpected error: %s", s, err)
		}
		if got != want {
			t.Fatalf("ParseDuration(%q): got %s; want %s", s, got, want)
		}
	}
	f("5m", 5*time.Minute)
	f("1h30m", 90*time.Minute)
	f("1.5h", 90*time.Minute)
	f("2h-5m", 115*time.Minute)
	f("-2h5m", -125*time.Minute)
	f("100ms", 100*time.Millisecond)
	f("1y", 365*24*time.Hour)
	f("1W", 7*24*time.Hour)
	f("300", 300*time.Second)

	for _, s := range []string{"", "5M", "5MB", "5x", "h"} {
		if _, err := ParseDuration(s); err == nil {
			t.Fatalf("ParseDuration(%q): expecting error", s)
		}
	}
}

func TestDaysForArgs(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC)
	f := func(start, end string, localOffset int64, maxDays int, want []string, truncated bool) {
		t.Helper()
		r, err := DaysForArgs(start, end, now, localOffset, maxDays)
		if err != nil {
			t.Fatalf("DaysForArgs(%q, %q): unexpected error: %s", start, end, err)
		}
		if !slices.Equal(r.Days, want) {
			t.Fatalf("DaysForArgs(%q, %q): got %v; want %v", start, end, r.Days, want)
		}
		if r.Truncated != truncated {
			t.Fatalf("DaysForArgs(%q, %q): got truncated=%v; want %v", start, end, r.Truncated, truncated)
		}
	}

	// No args - nothing to restore.
	f("", "", 0, 31, nil, false)

	// A single day.
	f("2026-11-04T00:00:00Z", "2026-11-04T23:59:59Z", 0, 31, []string{"20261104"}, false)
	// end is exclusive, so midnight of the next day isn't included.
	f("2026-11-04T00:00:00Z", "2026-11-05T00:00:00Z", 0, 31, []string{"20261104"}, false)
	f("2026-11-04", "2026-11-05", 0, 31, []string{"20261104"}, false)
	// Two days.
	f("2026-11-03T12:00:00Z", "2026-11-04T01:00:00Z", 0, 31, []string{"20261103", "20261104"}, false)
	// Timezone offsets are taken into account.
	f("2026-11-04T01:00:00+08:00", "2026-11-04T02:00:00+08:00", 0, 31, []string{"20261103"}, false)
	// Local timezone is used for values without timezone.
	f("2026-11-04", "2026-11-05", 8*3600*1e9, 31, []string{"20261103", "20261104"}, false)

	// Missing end means now.
	f("2026-11-03T10:00:00Z", "", 0, 31, []string{"20261103", "20261104", "20261105"}, false)
	f("1d", "", 0, 31, []string{"20261104", "20261105"}, false)
	f("5m", "", 0, 31, []string{"20261105"}, false)
	// Missing start means only the day of end.
	f("", "2026-11-03T10:00:00Z", 0, 31, []string{"20261103"}, false)
	f("", "2026-11-04T00:00:00Z", 0, 31, []string{"20261103"}, false)

	// Unix timestamps in seconds and milliseconds.
	s := time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC).Unix()
	e := time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC).Unix()
	f(itoa(s), itoa(e), 0, 31, []string{"20261103", "20261104"}, false)
	f(itoa(s*1000), itoa(e*1000), 0, 31, []string{"20261103", "20261104"}, false)
	f(itoa(s)+".5", itoa(e)+".25", 0, 31, []string{"20261103", "20261104"}, false)

	// Days in the future are dropped.
	f("2026-11-04T00:00:00Z", "2026-11-20T00:00:00Z", 0, 31, []string{"20261104", "20261105"}, false)
	f("2026-11-10T00:00:00Z", "2026-11-20T00:00:00Z", 0, 31, nil, false)

	// end before start.
	f("2026-11-04T00:00:00Z", "2026-11-03T00:00:00Z", 0, 31, nil, false)

	// Cap: only the most recent maxDays days are returned.
	f("2026-10-01T00:00:00Z", "2026-11-04T12:00:00Z", 0, 3, []string{"20261102", "20261103", "20261104"}, true)
	f("2026-11-02T00:00:00Z", "2026-11-04T12:00:00Z", 0, 3, []string{"20261102", "20261103", "20261104"}, false)
	f("1970", "", 0, 2, []string{"20261104", "20261105"}, true)

	// Across a month boundary.
	f("2026-10-31T22:00:00Z", "2026-11-01T02:00:00Z", 0, 31, []string{"20261031", "20261101"}, false)

	// Parse errors are reported.
	for _, args := range [][2]string{{"foo", ""}, {"", "bar"}, {"2026-11-04", "2026-13-01"}} {
		if _, err := DaysForArgs(args[0], args[1], now, 0, 31); err == nil {
			t.Fatalf("DaysForArgs(%q, %q): expecting error", args[0], args[1])
		}
	}
}

func TestDaysForArgsRequestedDays(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC)
	r, err := DaysForArgs("2026-10-01T00:00:00Z", "2026-11-04T12:00:00Z", now, 0, 31)
	if err != nil {
		t.Fatal(err)
	}
	if r.RequestedDays != 35 || len(r.Days) != 31 || !r.Truncated {
		t.Fatalf("unexpected result: requested=%d days=%d truncated=%v", r.RequestedDays, len(r.Days), r.Truncated)
	}
	if r.Days[0] != "20261005" || r.Days[30] != "20261104" {
		t.Fatalf("unexpected days: %v", r.Days)
	}
}

func TestIsPartitionName(t *testing.T) {
	for _, s := range []string{"20261104", "19700101", "20240229"} {
		if !IsPartitionName(s) {
			t.Fatalf("expecting %q to be a valid partition name", s)
		}
	}
	for _, s := range []string{"", "2026110", "202611041", "2026-11-04", "20261332", "20230229", "abcdefgh", "../../12"} {
		if IsPartitionName(s) {
			t.Fatalf("expecting %q to be an invalid partition name", s)
		}
	}
}

func TestDayName(t *testing.T) {
	if got := DayName(0); got != "19700101" {
		t.Fatalf("got %q", got)
	}
	day := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC).Unix() / 86400
	if got := DayName(day); got != "20261104" {
		t.Fatalf("got %q", got)
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
