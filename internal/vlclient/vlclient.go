// Package vlclient is a minimal client for the VictoriaLogs partition management HTTP API.
//
// Endpoints used:
//
//	GET /health
//	GET /internal/partition/list
//	GET /internal/partition/attach?name=YYYYMMDD
//	GET /internal/partition/detach?name=YYYYMMDD
//	GET /internal/partition/snapshot/list
//	GET /internal/partition/snapshot/create?partition_prefix=YYYYMMDD
//	GET /internal/partition/snapshot/delete?path=<abs snapshot path>
package vlclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to a single VictoriaLogs instance.
type Client struct {
	base    *url.URL
	authKey string
	hc      *http.Client
}

// HTTPError is returned when VictoriaLogs responds with a non-2xx status code.
type HTTPError struct {
	URL        string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("unexpected status code %d from %s: %s", e.StatusCode, e.URL, e.Body)
}

// IsStatus reports whether err is an *HTTPError with the given status code.
func IsStatus(err error, code int) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.StatusCode == code
}

// New creates a client for baseURL such as http://127.0.0.1:9428.
// authKey is passed as the authKey query arg to /internal/partition/* when non-empty
// (see -partitionManageAuthKey in VictoriaLogs).
func New(baseURL, authKey string) (*Client, error) {
	if !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("cannot parse VictoriaLogs address %q: %w", baseURL, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("missing host in VictoriaLogs address %q", baseURL)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery = ""
	return &Client{
		base:    u,
		authKey: authKey,
		hc:      &http.Client{Timeout: 10 * time.Minute},
	}, nil
}

// BaseURL returns the base URL of the VictoriaLogs instance.
func (c *Client) BaseURL() string { return c.base.String() }

func (c *Client) get(ctx context.Context, path string, args url.Values) ([]byte, error) {
	u := *c.base
	u.Path += path
	if args == nil {
		args = url.Values{}
	}
	if c.authKey != "" && strings.HasPrefix(path, "/internal/") {
		args.Set("authKey", c.authKey)
	}
	u.RawQuery = args.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot request %s: %w", redact(u), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("cannot read response from %s: %w", redact(u), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &HTTPError{URL: redact(u), StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	return body, nil
}

func redact(u url.URL) string {
	q := u.Query()
	if q.Has("authKey") {
		q.Set("authKey", "secret")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// Ping checks that VictoriaLogs is reachable and healthy.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.get(ctx, "/health", nil)
	return err
}

// parseStringList parses a JSON string or a JSON array of strings. An empty body yields nil.
func parseStringList(body []byte) ([]string, error) {
	s := strings.TrimSpace(string(body))
	if s == "" || s == "null" {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal([]byte(s), &list); err == nil {
		return list, nil
	}
	var one string
	if err := json.Unmarshal([]byte(s), &one); err == nil {
		if one == "" {
			return nil, nil
		}
		return []string{one}, nil
	}
	return nil, fmt.Errorf("cannot parse response %q as a JSON string or array of strings", s)
}

// ForceFlush makes recently ingested in-memory data searchable and persisted
// before taking a snapshot. Errors may be ignored by callers on versions without this endpoint.
func (c *Client) ForceFlush(ctx context.Context) error {
	_, err := c.get(ctx, "/internal/force_flush", nil)
	return err
}

// SnapshotList returns absolute paths of all partition snapshots.
func (c *Client) SnapshotList(ctx context.Context) ([]string, error) {
	body, err := c.get(ctx, "/internal/partition/snapshot/list", nil)
	if err != nil {
		return nil, err
	}
	return parseStringList(body)
}

// SnapshotCreate creates snapshots for partitions whose names start with partitionPrefix.
// It returns snapshot paths reported by VictoriaLogs (possibly empty for versions that
// do not return them; use SnapshotList to discover them in that case).
//
// Both partition_prefix (newer VictoriaLogs) and name (older VictoriaLogs) are sent.
func (c *Client) SnapshotCreate(ctx context.Context, partitionPrefix string) ([]string, error) {
	body, err := c.get(ctx, "/internal/partition/snapshot/create", url.Values{
		"partition_prefix": {partitionPrefix},
		"name":             {partitionPrefix},
	})
	if err != nil {
		return nil, err
	}
	paths, perr := parseStringList(body)
	if perr != nil {
		// The snapshot was created, but the response format is unknown. Let the caller rely on SnapshotList.
		return nil, nil
	}
	return paths, nil
}

// SnapshotDelete deletes the snapshot at the given absolute path.
func (c *Client) SnapshotDelete(ctx context.Context, path string) error {
	_, err := c.get(ctx, "/internal/partition/snapshot/delete", url.Values{"path": {path}})
	return err
}

// PartitionList returns names of attached partitions.
func (c *Client) PartitionList(ctx context.Context) ([]string, error) {
	body, err := c.get(ctx, "/internal/partition/list", nil)
	if err != nil {
		return nil, err
	}
	return parseStringList(body)
}

// PartitionAttach attaches the partition directory <storageDataPath>/partitions/<name>.
func (c *Client) PartitionAttach(ctx context.Context, name string) error {
	_, err := c.get(ctx, "/internal/partition/attach", url.Values{"name": {name}})
	return err
}

// PartitionDetach detaches the partition with the given name.
func (c *Client) PartitionDetach(ctx context.Context, name string) error {
	_, err := c.get(ctx, "/internal/partition/detach", url.Values{"name": {name}})
	return err
}
