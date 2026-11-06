package restore

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/restore/vltime"
)

// retentionRefreshInterval is how often the retention of an external VictoriaLogs is re-read.
const retentionRefreshInterval = 10 * time.Minute

// msecsPer31Days is the length of a "month" in VictoriaLogs retention flags.
const msecsPer31Days = 31 * 24 * 3600 * 1000

// parseRetention parses the value of the VictoriaLogs -retentionPeriod flag.
// A value without a unit is the number of 31-day months, like in VictoriaLogs
// (see lib/flagutil.RetentionDuration).
func parseRetention(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty retention")
	}
	if months, err := strconv.ParseFloat(s, 64); err == nil {
		if months <= 0 {
			return 0, fmt.Errorf("retention must be positive; got %q", s)
		}
		return time.Duration(months*msecsPer31Days) * time.Millisecond, nil
	}
	s = strings.ToLower(s)
	if strings.HasSuffix(s, "m") {
		return 0, fmt.Errorf("retention in months must be set without the m suffix; got %q", s)
	}
	d, err := vltime.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("retention must be positive; got %q", s)
	}
	return d, nil
}

// parseRetentionFromMetrics extracts -retentionPeriod from the VictoriaLogs /metrics
// output, where flags are exposed as flag{name="retentionPeriod", value="7d", is_set="true"} 1.
func parseRetentionFromMetrics(r io.Reader) (time.Duration, error) {
	const prefix = `flag{name="retentionPeriod", value="`
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := line[len(prefix):]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			return 0, fmt.Errorf("cannot parse metrics line %q", line)
		}
		return parseRetention(rest[:end])
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New(`flag{name="retentionPeriod"} metric is missing`)
}

// fetchRetention reads -retentionPeriod of the VictoriaLogs at baseURL from its /metrics page.
func fetchRetention(ctx context.Context, baseURL string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(baseURL, "/")+"/metrics", nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("unexpected status code %d from %s/metrics", resp.StatusCode, baseURL)
	}
	return parseRetentionFromMetrics(resp.Body)
}

// minRetainedDay returns the first day number (since the unix epoch) VictoriaLogs
// keeps with the given retention; older partitions are dropped by VictoriaLogs
// (see Storage.getMinAllowedDay in lib/logstorage).
func minRetainedDay(now time.Time, retention time.Duration) int64 {
	return now.UTC().Add(-retention).UnixNano() / (24 * int64(time.Hour))
}

// dayNumber returns the day number since the unix epoch for the partition name day (YYYYMMDD).
func dayNumber(day string) (int64, error) {
	t, err := time.Parse(vltime.PartitionNameLayout, day)
	if err != nil {
		return 0, err
	}
	return t.Unix() / (24 * 3600), nil
}
