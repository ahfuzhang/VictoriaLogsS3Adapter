package restore

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/vlclient"
)

// partitionAPI is the subset of the VictoriaLogs management API used by the server.
// It is implemented by *vlclient.Client.
type partitionAPI interface {
	Ping(ctx context.Context) error
	PartitionAttach(ctx context.Context, name string) error
	PartitionList(ctx context.Context) ([]string, error)
}

// backend is a VictoriaLogs instance requests are proxied to.
type backend struct {
	// baseURL is the normalized base URL such as http://127.0.0.1:9428.
	baseURL string
	// host is host:port of the instance.
	host string
	// basePath is an optional path prefix of the instance (without trailing slash).
	basePath string
	isTLS    bool
	hc       *fasthttp.HostClient
	vl       partitionAPI
}

// newBackend creates a backend for rawURL such as http://127.0.0.1:9428 or 127.0.0.1:9428.
func newBackend(rawURL, authKey string) (*backend, error) {
	if !strings.Contains(rawURL, "://") {
		rawURL = "http://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("cannot parse VictoriaLogs address %q: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme in VictoriaLogs address %q; must be http or https", rawURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("missing host in VictoriaLogs address %q", rawURL)
	}
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		if u.Scheme == "https" {
			host = net.JoinHostPort(host, "443")
		} else {
			host = net.JoinHostPort(host, "80")
		}
	}
	basePath := strings.TrimSuffix(u.Path, "/")
	baseURL := u.Scheme + "://" + u.Host + basePath
	vl, err := vlclient.New(baseURL, authKey)
	if err != nil {
		return nil, err
	}
	b := &backend{
		baseURL:  baseURL,
		host:     host,
		basePath: basePath,
		isTLS:    u.Scheme == "https",
		vl:       vl,
	}
	b.hc = &fasthttp.HostClient{
		Addr:  host,
		IsTLS: b.isTLS,
		// Queries over big time ranges and /select/logsql/tail may take arbitrary time,
		// so there are no read and write timeouts. Response bodies are streamed.
		StreamResponseBody:       true,
		MaxConns:                 1024,
		MaxIdleConnDuration:      30 * time.Second,
		ReadBufferSize:           64 * 1024,
		WriteBufferSize:          64 * 1024,
		NoDefaultUserAgentHeader: true,
		DialDualStack:            true,
	}
	return b, nil
}

func (b *backend) close() {
	b.hc.CloseIdleConnections()
}
