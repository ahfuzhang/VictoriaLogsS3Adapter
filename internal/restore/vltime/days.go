package vltime

import (
	"fmt"
	"math"
	"time"
)

const nsecsPerDay = 24 * 3600 * int64(time.Second)

// PartitionNameLayout is the Go time layout of VictoriaLogs per-day partition names.
const PartitionNameLayout = "20060102"

// DayRange is the result of DaysForArgs.
type DayRange struct {
	// Days holds partition names (YYYYMMDD, UTC) in ascending order.
	Days []string
	// Truncated is true when the requested range covered more than maxDays days
	// and only the most recent maxDays days were returned.
	Truncated bool
	// RequestedDays is the number of days covered by the request before truncation.
	RequestedDays int64
}

// DaysForArgs returns the UTC days (partition names) touched by a VictoriaLogs
// select request with the given raw start and end query args.
//
// The semantics follow VictoriaLogs: end is exclusive, values are parsed by ParseTimeAt.
//   - both args empty: no days.
//   - end empty: end is now.
//   - start empty: only the day of end.
//
// Days after the current UTC day are never returned, since backups cannot exist for them.
// If the range covers more than maxDays days, only the most recent maxDays days are returned.
// An error is returned if start or end cannot be parsed.
func DaysForArgs(startArg, endArg string, now time.Time, localOffsetNsecs int64, maxDays int) (DayRange, error) {
	var r DayRange
	if startArg == "" && endArg == "" {
		return r, nil
	}
	nowNsecs := now.UnixNano()
	end := nowNsecs
	if endArg != "" {
		e, err := ParseTimeAt(endArg, nowNsecs, localOffsetNsecs)
		if err != nil {
			return r, fmt.Errorf("cannot parse end=%q: %w", endArg, err)
		}
		// VictoriaLogs treats end as exclusive: [start, end)
		if e != math.MinInt64 {
			e--
		}
		end = e
	}
	start := end
	if startArg != "" {
		s, err := ParseTimeAt(startArg, nowNsecs, localOffsetNsecs)
		if err != nil {
			return r, fmt.Errorf("cannot parse start=%q: %w", startArg, err)
		}
		start = s
	}
	firstDay := floorDiv(start, nsecsPerDay)
	lastDay := min(floorDiv(end, nsecsPerDay), floorDiv(nowNsecs, nsecsPerDay))
	if lastDay < firstDay {
		return r, nil
	}
	r.RequestedDays = lastDay - firstDay + 1
	if maxDays > 0 && r.RequestedDays > int64(maxDays) {
		firstDay = lastDay - int64(maxDays) + 1
		r.Truncated = true
	}
	r.Days = make([]string, 0, lastDay-firstDay+1)
	for d := firstDay; d <= lastDay; d++ {
		r.Days = append(r.Days, DayName(d))
	}
	return r, nil
}

// DayName returns the partition name for the given day number since the unix epoch.
func DayName(day int64) string {
	return time.Unix(day*(nsecsPerDay/int64(time.Second)), 0).UTC().Format(PartitionNameLayout)
}

// IsPartitionName reports whether s is a valid partition name (YYYYMMDD).
func IsPartitionName(s string) bool {
	if len(s) != len(PartitionNameLayout) {
		return false
	}
	t, err := time.Parse(PartitionNameLayout, s)
	return err == nil && t.Format(PartitionNameLayout) == s
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
