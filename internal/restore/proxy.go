package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/restore/vltime"
)

// DayEnsurer makes partitions for the given days available locally.
type DayEnsurer interface {
	EnsureDays(ctx context.Context, days []string)
}

// tailPath is the streaming endpoint which may stay open forever. Such requests
// wait while VictoriaLogs is restarting, but they never delay a restart.
const tailPath = "/select/logsql/tail"

// selectPathPrefix is the prefix of VictoriaLogs query APIs which accept start and end args.
const selectPathPrefix = "/select/logsql/"

// hopByHopHeaders are removed when proxying requests and responses (RFC 9110, section 7.6.1).
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// ProxyConfig configures a Proxy.
type ProxyConfig struct {
	// Prefix is the normalized -http.prefix (see NormalizePrefix).
	Prefix string
	// Restorer restores partitions before select requests are proxied. May be nil.
	Restorer DayEnsurer
	// MaxDays is the maximum number of days a single request may restore.
	MaxDays int
	// GateWaitTimeout is the maximum time a request waits while VictoriaLogs is restarting.
	GateWaitTimeout time.Duration
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// LocalOffset returns the local timezone offset in nanoseconds.
	// Defaults to vltime.LocalTimezoneOffsetNsecs.
	LocalOffset func() int64
}

// Proxy forwards HTTP requests to VictoriaLogs and restores partitions from
// backups on demand.
type Proxy struct {
	cfg     ProxyConfig
	backend atomic.Pointer[backend]
	gate    *gate

	// streams holds response bodies currently streamed from VictoriaLogs.
	streamsMu sync.Mutex
	streams   map[*backendBody]struct{}
}

// NewProxy creates a proxy. setBackend must be called before serving requests.
func NewProxy(cfg ProxyConfig) *Proxy {
	if cfg.GateWaitTimeout <= 0 {
		cfg.GateWaitTimeout = 5 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.LocalOffset == nil {
		cfg.LocalOffset = vltime.LocalTimezoneOffsetNsecs
	}
	return &Proxy{cfg: cfg, gate: newGate(), streams: make(map[*backendBody]struct{})}
}

// errStreamInterrupted is passed to backend streams closed by closeStreams.
var errStreamInterrupted = errors.New("the response stream has been interrupted, since VictoriaLogs is restarting or stopping")

// closeStreams interrupts response bodies currently streamed from VictoriaLogs,
// so VictoriaLogs can stop without waiting for them. If tailOnly is set, only
// /select/logsql/tail streams are interrupted. It returns the number of interrupted streams.
func (p *Proxy) closeStreams(tailOnly bool) int {
	p.streamsMu.Lock()
	var bodies []*backendBody
	for b := range p.streams {
		if !tailOnly || b.tail {
			bodies = append(bodies, b)
		}
	}
	p.streamsMu.Unlock()
	for _, b := range bodies {
		_ = b.CloseWithError(errStreamInterrupted)
	}
	return len(bodies)
}

// setBackend switches proxying to b.
func (p *Proxy) setBackend(b *backend) {
	if old := p.backend.Swap(b); old != nil && old != b {
		old.close()
	}
}

// currentBackend returns the current backend or nil.
func (p *Proxy) currentBackend() *backend {
	return p.backend.Load()
}

// NormalizePrefix normalizes the -http.prefix value: "/admin", "/admin/" and
// "admin/" all become "/admin"; "" and "/" become "".
func NormalizePrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	for strings.Contains(prefix, "//") {
		prefix = strings.ReplaceAll(prefix, "//", "/")
	}
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return ""
	}
	return "/" + prefix
}

// StripPrefix removes the normalized prefix from path. ok is false if path
// is not under prefix.
func StripPrefix(path, prefix string) (string, bool) {
	if prefix == "" {
		return path, true
	}
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rest := path[len(prefix):]
	if rest == "" {
		return "/", true
	}
	if rest[0] != '/' {
		return "", false
	}
	return rest, true
}

// Handler is the fasthttp request handler.
func (p *Proxy) Handler(ctx *fasthttp.RequestCtx) {
	path, ok := StripPrefix(string(ctx.Path()), p.cfg.Prefix)
	if !ok {
		ctx.Error("404 page not found", fasthttp.StatusNotFound)
		return
	}
	if p.cfg.Restorer != nil && strings.HasPrefix(path, selectPathPrefix) {
		p.restoreForRequest(ctx, path)
	}
	p.forward(ctx, path)
}

// restoreForRequest restores partitions for the days covered by the start and end args of the request.
func (p *Proxy) restoreForRequest(ctx *fasthttp.RequestCtx, path string) {
	start := formValue(ctx, "start")
	end := formValue(ctx, "end")
	if start == "" && end == "" {
		return
	}
	r, err := vltime.DaysForArgs(start, end, p.cfg.Now(), p.cfg.LocalOffset(), p.cfg.MaxDays)
	if err != nil {
		// VictoriaLogs reports the error to the client.
		slog.Debug("cannot determine the requested time range; forwarding the request as is", "path", path, "error", err)
		return
	}
	if len(r.Days) == 0 {
		return
	}
	if r.Truncated {
		slog.Warn("the requested time range exceeds -restore.max.days; restoring only the most recent days",
			"path", path, "start", start, "end", end, "requested_days", r.RequestedDays, "max_days", p.cfg.MaxDays,
			"first_restored_day", r.Days[0], "last_restored_day", r.Days[len(r.Days)-1])
	}
	p.cfg.Restorer.EnsureDays(ctx, r.Days)
}

// formValue returns the named arg like net/http Request.FormValue used by VictoriaLogs:
// args from an url-encoded POST, PUT or PATCH body take precedence over query args.
func formValue(ctx *fasthttp.RequestCtx, name string) string {
	if hasFormBody(ctx) {
		if args := ctx.PostArgs(); args.Has(name) {
			return string(args.Peek(name))
		}
	}
	return string(ctx.QueryArgs().Peek(name))
}

func hasFormBody(ctx *fasthttp.RequestCtx) bool {
	if !ctx.IsPost() && !ctx.IsPut() && !ctx.IsPatch() {
		return false
	}
	ct := ctx.Request.Header.ContentType()
	if i := bytes.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return bytes.EqualFold(bytes.TrimSpace(ct), []byte("application/x-www-form-urlencoded"))
}

// forward proxies the request to the current backend.
func (p *Proxy) forward(ctx *fasthttp.RequestCtx, path string) {
	if !p.gate.enter(ctx, p.cfg.GateWaitTimeout) {
		ctx.Error("VictoriaLogs is restarting; try again later", fasthttp.StatusServiceUnavailable)
		return
	}
	var leaveOnce sync.Once
	leave := func() { leaveOnce.Do(p.gate.leave) }

	be := p.currentBackend()
	if be == nil {
		leave()
		ctx.Error("VictoriaLogs is not available", fasthttp.StatusServiceUnavailable)
		return
	}
	if path == tailPath {
		// Live tailing may never finish, so it must not delay restarts. A restart
		// breaks the stream and the client reconnects.
		leave()
	}

	req := fasthttp.AcquireRequest()
	ctx.Request.Header.CopyTo(&req.Header)
	if ctx.IsGet() || ctx.IsHead() {
		// fasthttp refuses to send bodies with GET and HEAD requests, and VictoriaLogs ignores them anyway.
		// Drop the framing headers copied from the client too: otherwise the backend waits forever
		// for a body that never comes.
		req.Header.Del(fasthttp.HeaderContentLength)
		req.Header.Del(fasthttp.HeaderTransferEncoding)
	} else {
		// The body is not copied; ctx.Request outlives the backend call.
		req.SetBodyRaw(ctx.Request.Body())
	}
	uri := req.URI()
	if be.isTLS {
		uri.SetScheme("https")
	} else {
		uri.SetScheme("http")
	}
	uri.SetHost(be.host)
	uri.SetPath(be.basePath + path)
	uri.SetQueryStringBytes(ctx.URI().QueryString())
	for _, h := range hopByHopHeaders {
		req.Header.Del(h)
	}
	req.Header.Del("Expect")
	req.Header.ResetConnectionClose()
	req.Header.Set("X-Forwarded-For", appendForwardedFor(ctx))
	req.Header.SetBytesV("X-Forwarded-Host", ctx.Host())
	if ctx.IsTLS() {
		req.Header.Set("X-Forwarded-Proto", "https")
	} else {
		req.Header.Set("X-Forwarded-Proto", "http")
	}

	startTime := time.Now()
	resp := fasthttp.AcquireResponse()
	err := be.hc.Do(req, resp)
	fasthttp.ReleaseRequest(req)
	if err != nil {
		fasthttp.ReleaseResponse(resp)
		leave()
		slog.Warn("cannot proxy request to VictoriaLogs", "path", path, "backend", be.baseURL, "error", err)
		ctx.Error("cannot proxy request to VictoriaLogs: "+err.Error(), fasthttp.StatusBadGateway)
		return
	}

	resp.Header.CopyTo(&ctx.Response.Header)
	ctx.Response.Header.ResetConnectionClose()
	for _, h := range hopByHopHeaders {
		ctx.Response.Header.Del(h)
	}
	slog.Debug("proxied request", "method", string(ctx.Method()), "path", path, "status", resp.StatusCode(),
		"header_duration_seconds", time.Since(startTime).Seconds())

	if !resp.IsBodyStream() {
		if !ctx.IsHead() {
			ctx.Response.SetBody(resp.Body())
		}
		fasthttp.ReleaseResponse(resp)
		leave()
		return
	}
	contentLength := resp.Header.ContentLength()
	if contentLength < 0 {
		contentLength = -1
	}
	bb := &backendBody{r: resp.BodyStream(), resp: resp, tail: path == tailPath}
	bb.onClose = func() {
		p.streamsMu.Lock()
		delete(p.streams, bb)
		p.streamsMu.Unlock()
		leave()
	}
	p.streamsMu.Lock()
	p.streams[bb] = struct{}{}
	p.streamsMu.Unlock()
	ctx.Response.SetBodyStream(bb, contentLength)
}

func appendForwardedFor(ctx *fasthttp.RequestCtx) string {
	ip := ctx.RemoteIP().String()
	if prior := ctx.Request.Header.Peek("X-Forwarded-For"); len(prior) > 0 {
		return string(prior) + ", " + ip
	}
	return ip
}

// backendBody streams a backend response body to the client and releases the
// backend response when the server is done with it.
type backendBody struct {
	r       io.Reader
	resp    *fasthttp.Response
	tail    bool
	onClose func()
	once    sync.Once
}

func (b *backendBody) Read(p []byte) (int, error) {
	return b.r.Read(p)
}

// CloseWithError implements fasthttp.ReadCloserWithError. fasthttp calls it after
// the response is written (err is nil) or when writing fails. closeStreams calls it
// concurrently with Read in order to interrupt the stream; the fasthttp stream
// reader supports this and makes subsequent reads fail.
func (b *backendBody) CloseWithError(err error) error {
	b.once.Do(func() {
		if c, ok := b.r.(fasthttp.ReadCloserWithError); ok {
			_ = c.CloseWithError(err)
		}
		fasthttp.ReleaseResponse(b.resp)
		b.onClose()
	})
	return nil
}
