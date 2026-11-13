package restore

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

func TestNormalizePrefix(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"/":          "",
		"//":         "",
		"/admin":     "/admin",
		"/admin/":    "/admin",
		"admin/":     "/admin",
		"admin":      "/admin",
		" /admin/ ":  "/admin",
		"//admin//x": "/admin/x",
		"/a/b/":      "/a/b",
	}
	for in, want := range cases {
		if got := NormalizePrefix(in); got != want {
			t.Fatalf("NormalizePrefix(%q): got %q; want %q", in, got, want)
		}
	}
}

func TestStripPrefix(t *testing.T) {
	f := func(path, prefix, want string, wantOK bool) {
		t.Helper()
		got, ok := StripPrefix(path, prefix)
		if ok != wantOK || got != want {
			t.Fatalf("StripPrefix(%q, %q): got (%q, %v); want (%q, %v)", path, prefix, got, ok, want, wantOK)
		}
	}
	f("/select/logsql/query", "", "/select/logsql/query", true)
	f("/admin/select/logsql/query", "/admin", "/select/logsql/query", true)
	f("/admin/", "/admin", "/", true)
	f("/admin", "/admin", "/", true)
	f("/administrator/select", "/admin", "", false)
	f("/select/logsql/query", "/admin", "", false)
	f("/", "/admin", "", false)
	f("/a/b/c", "/a/b", "/c", true)
}

func TestGate(t *testing.T) {
	g := newGate()
	if !g.enter(context.Background(), time.Second) {
		t.Fatal("enter must succeed on an open gate")
	}
	closed := make(chan int)
	go func() { closed <- g.close(5 * time.Second) }()
	waitFor(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.closed
	})
	// New requests wait while the gate is closed.
	entered := make(chan bool)
	go func() { entered <- g.enter(context.Background(), 5*time.Second) }()
	select {
	case <-entered:
		t.Fatal("enter must block while the gate is closed")
	case <-closed:
		t.Fatal("close must wait for in-flight requests")
	case <-time.After(50 * time.Millisecond):
	}
	g.leave()
	if n := <-closed; n != 0 {
		t.Fatalf("unexpected in-flight requests: %d", n)
	}
	g.open()
	if !<-entered {
		t.Fatal("enter must succeed after the gate is opened")
	}
	g.leave()

	// close returns after the timeout if requests are stuck.
	g.enter(context.Background(), time.Second)
	if n := g.close(20 * time.Millisecond); n != 1 {
		t.Fatalf("expecting 1 stuck request; got %d", n)
	}
	// enter times out while the gate is closed.
	if g.enter(context.Background(), 20*time.Millisecond) {
		t.Fatal("enter must time out")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if g.enter(ctx, time.Second) {
		t.Fatal("enter must fail on a canceled context")
	}
	g.open()
	g.leave()
	if n := g.inFlight(); n != 0 {
		t.Fatalf("unexpected in-flight requests: %d", n)
	}
}

// recordingEnsurer records EnsureDays calls.
type recordingEnsurer struct {
	mu    sync.Mutex
	calls [][]string
	// err is returned from EnsureDays.
	err error
}

func (r *recordingEnsurer) EnsureDays(_ context.Context, days []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, days)
	return r.err
}

func (r *recordingEnsurer) take() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.calls
	r.calls = nil
	return c
}

// testEnv runs a fake VictoriaLogs backend and the proxy on in-memory listeners.
type testEnv struct {
	proxy    *Proxy
	ensurer  *recordingEnsurer
	client   *fasthttp.HostClient
	feLn     *fasthttputil.InmemoryListener
	lastReq  atomic.Pointer[fasthttp.Request]
	tailCh   chan string
	attached []string
	mu       sync.Mutex
	failAtt  atomic.Bool
}

func newTestEnv(t *testing.T, prefix string) *testEnv {
	t.Helper()
	env := &testEnv{ensurer: &recordingEnsurer{}, tailCh: make(chan string)}

	beLn := fasthttputil.NewInmemoryListener()
	beSrv := &fasthttp.Server{Handler: env.backendHandler}
	go func() { _ = beSrv.Serve(beLn) }()
	t.Cleanup(func() { _ = beSrv.Shutdown() })

	env.proxy = NewProxy(ProxyConfig{
		Prefix:      NormalizePrefix(prefix),
		Restorer:    env.ensurer,
		MaxDays:     31,
		Now:         func() time.Time { return time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC) },
		LocalOffset: func() int64 { return 0 },
	})
	env.proxy.setBackend(newInmemoryBackend(t, beLn))

	feLn := fasthttputil.NewInmemoryListener()
	env.feLn = feLn
	feSrv := &fasthttp.Server{Handler: env.proxy.Handler, NoDefaultServerHeader: true, NoDefaultContentType: true}
	go func() { _ = feSrv.Serve(feLn) }()
	t.Cleanup(func() { _ = feSrv.Shutdown() })
	env.client = &fasthttp.HostClient{
		Addr:               "proxy:80",
		Dial:               func(string) (net.Conn, error) { return feLn.Dial() },
		StreamResponseBody: true,
	}
	return env
}

func newInmemoryBackend(t *testing.T, ln *fasthttputil.InmemoryListener) *backend {
	t.Helper()
	be, err := newBackend("http://vl:9428", "")
	if err != nil {
		t.Fatal(err)
	}
	be.hc.Dial = func(string) (net.Conn, error) { return ln.Dial() }
	be.vl = &fakePartitionAPI{hc: be.hc}
	return be
}

// fakePartitionAPI calls the partition management API of the in-memory backend.
type fakePartitionAPI struct {
	hc *fasthttp.HostClient
}

func (f *fakePartitionAPI) get(path string) (int, string, error) {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI("http://vl:9428" + path)
	if err := f.hc.DoTimeout(req, resp, 5*time.Second); err != nil {
		return 0, "", err
	}
	b, err := io.ReadAll(resp.BodyStream())
	if resp.BodyStream() == nil {
		b, err = resp.Body(), nil
	}
	return resp.StatusCode(), string(b), err
}

func (f *fakePartitionAPI) Ping(context.Context) error {
	_, _, err := f.get("/health")
	return err
}

func (f *fakePartitionAPI) PartitionAttach(_ context.Context, name string) error {
	status, body, err := f.get("/internal/partition/attach?name=" + url.QueryEscape(name))
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("unexpected status %d: %s", status, body)
	}
	return nil
}

func (f *fakePartitionAPI) PartitionList(context.Context) ([]string, error) {
	status, body, err := f.get("/internal/partition/list")
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("unexpected status %d: %s", status, body)
	}
	var names []string
	if err := json.Unmarshal([]byte(body), &names); err != nil {
		return nil, err
	}
	return names, nil
}

func (env *testEnv) backendHandler(ctx *fasthttp.RequestCtx) {
	req := &fasthttp.Request{}
	ctx.Request.CopyTo(req)
	env.lastReq.Store(req)
	switch string(ctx.Path()) {
	case "/select/logsql/tail":
		ctx.SetContentType("application/stream+json")
		ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
			// VictoriaLogs flushes the response headers immediately; fasthttp sends
			// them with the first chunk, so emit a first line right away.
			_, _ = w.WriteString(`{"started":true}` + "\n")
			if err := w.Flush(); err != nil {
				return
			}
			for line := range env.tailCh {
				_, _ = w.WriteString(line + "\n")
				if err := w.Flush(); err != nil {
					return
				}
			}
		})
	case "/big":
		ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
			for i := range 1000 {
				fmt.Fprintf(w, "line %d %s\n", i, strings.Repeat("x", 100))
			}
		})
	case "/internal/partition/attach":
		if env.failAtt.Load() {
			ctx.Error("attach failed", fasthttp.StatusInternalServerError)
			return
		}
		env.mu.Lock()
		env.attached = append(env.attached, string(ctx.QueryArgs().Peek("name")))
		env.mu.Unlock()
	case "/internal/partition/list":
		env.mu.Lock()
		defer env.mu.Unlock()
		ctx.SetContentType("application/json")
		fmt.Fprintf(ctx, "[")
		for i, name := range env.attached {
			if i > 0 {
				fmt.Fprintf(ctx, ",")
			}
			fmt.Fprintf(ctx, "%q", name)
		}
		fmt.Fprintf(ctx, "]")
	default:
		ctx.Response.Header.Set("X-Backend-Path", string(ctx.Path()))
		ctx.Response.Header.Set("Keep-Alive", "timeout=5")
		ctx.SetContentType("application/json")
		fmt.Fprintf(ctx, `{"path":%q,"query":%q,"body":%q}`, ctx.Path(), ctx.URI().QueryString(), ctx.PostBody())
	}
}

func (env *testEnv) do(t *testing.T, method, uri, contentType, body string) (int, *fasthttp.ResponseHeader, string) {
	t.Helper()
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI("http://proxy" + uri)
	req.Header.SetMethod(method)
	if contentType != "" {
		req.Header.SetContentType(contentType)
	}
	req.SetBodyString(body)
	if err := env.client.DoTimeout(req, resp, 5*time.Second); err != nil {
		t.Fatalf("request %s %s failed: %s", method, uri, err)
	}
	b, err := io.ReadAll(resp.BodyStream())
	if resp.BodyStream() == nil {
		b, err = resp.Body(), nil
	}
	if err != nil {
		t.Fatalf("cannot read response body: %s", err)
	}
	h := &fasthttp.ResponseHeader{}
	resp.Header.CopyTo(h)
	return resp.StatusCode(), h, string(b)
}

func TestProxyPrefixAndForwarding(t *testing.T) {
	env := newTestEnv(t, "/admin/")

	status, h, body := env.do(t, "GET", "/admin/select/logsql/field_names?query=*&x=1", "", "")
	if status != 200 {
		t.Fatalf("unexpected status %d: %s", status, body)
	}
	if !strings.Contains(body, `"path":"/select/logsql/field_names"`) || !strings.Contains(body, `"query":"query=*&x=1"`) {
		t.Fatalf("unexpected backend request: %s", body)
	}
	if got := string(h.Peek("X-Backend-Path")); got != "/select/logsql/field_names" {
		t.Fatalf("response headers must be proxied; got X-Backend-Path=%q", got)
	}
	if got := h.Peek("Keep-Alive"); len(got) != 0 {
		t.Fatalf("hop-by-hop response headers must be removed; got Keep-Alive=%q", got)
	}
	if got := string(h.ContentType()); got != "application/json" {
		t.Fatalf("unexpected content type %q", got)
	}

	// Requests outside the prefix get 404 and never reach the backend.
	env.lastReq.Store(nil)
	for _, uri := range []string{"/select/logsql/query?query=*", "/administrator/x", "/"} {
		status, _, _ = env.do(t, "GET", uri, "", "")
		if status != 404 {
			t.Fatalf("%s: unexpected status %d; want 404", uri, status)
		}
	}
	if env.lastReq.Load() != nil {
		t.Fatalf("requests outside the prefix must not be proxied")
	}

	// POST bodies and headers are proxied; hop-by-hop request headers are removed.
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI("http://proxy/admin/insert/jsonline")
	req.Header.SetMethod("POST")
	req.Header.Set("AccountID", "12")
	req.Header.Set("Proxy-Authorization", "secret")
	req.Header.Set("Te", "trailers")
	req.SetBodyString(`{"_msg":"x"}`)
	if err := env.client.Do(req, resp); err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.BodyStream())
	last := env.lastReq.Load()
	if got := string(last.Header.Peek("AccountID")); got != "12" {
		t.Fatalf("headers must be proxied; got AccountID=%q", got)
	}
	if got := last.Header.Peek("Proxy-Authorization"); len(got) != 0 {
		t.Fatalf("Proxy-Authorization must not be proxied")
	}
	if got := last.Header.Peek("Te"); len(got) != 0 {
		t.Fatalf("Te must not be proxied")
	}
	if got := string(last.Body()); got != `{"_msg":"x"}` {
		t.Fatalf("unexpected body %q", got)
	}
	if got := string(last.Header.Peek("X-Forwarded-For")); got == "" {
		t.Fatalf("X-Forwarded-For must be set")
	}
	if n := env.proxy.gate.inFlight(); n != 0 {
		t.Fatalf("unexpected in-flight requests: %d", n)
	}
}

func TestProxyRestoresRequestedDays(t *testing.T) {
	env := newTestEnv(t, "")
	f := func(method, uri, contentType, body string, want [][]string) {
		t.Helper()
		status, _, respBody := env.do(t, method, uri, contentType, body)
		if status != 200 {
			t.Fatalf("unexpected status %d: %s", status, respBody)
		}
		got := env.ensurer.take()
		if !slices.EqualFunc(got, want, slices.Equal) {
			t.Fatalf("%s %s: unexpected EnsureDays calls %v; want %v", method, uri, got, want)
		}
	}
	q := func(start, end string) string {
		v := url.Values{"query": {"*"}}
		if start != "" {
			v.Set("start", start)
		}
		if end != "" {
			v.Set("end", end)
		}
		return v.Encode()
	}

	f("GET", "/select/logsql/query?"+q("2026-11-04T00:00:00Z", "2026-11-04T23:59:59Z"), "", "", [][]string{{"20261104"}})
	f("GET", "/select/logsql/hits?"+q("2026-11-03T12:00:00Z", "2026-11-04T12:00:00Z"), "", "", [][]string{{"20261103", "20261104"}})
	f("GET", "/select/logsql/stream_field_names?"+q("2026-11-03", ""), "", "", [][]string{{"20261103", "20261104", "20261105"}})
	// Other select endpoints with start/end are handled too.
	f("GET", "/select/logsql/field_values?field=app&"+q("1d", ""), "", "", [][]string{{"20261104", "20261105"}})
	// No start/end: nothing to restore.
	f("GET", "/select/logsql/query?query=*", "", "", nil)
	// Unparseable args are forwarded as is.
	f("GET", "/select/logsql/query?"+q("yesterday", ""), "", "", nil)
	// Non-select endpoints are not inspected.
	f("GET", "/internal/whatever?"+q("2026-11-04", ""), "", "", nil)
	// url-encoded POST body.
	f("POST", "/select/logsql/query", "application/x-www-form-urlencoded", q("2026-11-02T10:00:00Z", "2026-11-02T11:00:00Z"), [][]string{{"20261102"}})
	// POST body args take precedence over query args, like in net/http.
	f("POST", "/select/logsql/query?start=2026-11-01T00:00:00Z", "application/x-www-form-urlencoded; charset=utf-8",
		q("2026-11-02T10:00:00Z", "2026-11-02T11:00:00Z"), [][]string{{"20261102"}})
	// Query args are used if the body has no such args.
	f("POST", "/select/logsql/query?start=2026-11-01T10:00:00Z&end=2026-11-01T11:00:00Z", "application/x-www-form-urlencoded",
		"query=*", [][]string{{"20261101"}})
	// Non-form bodies are not parsed.
	f("POST", "/select/logsql/query", "application/json", `start=2026-11-02T10:00:00Z`, nil)
}

func TestProxyRestoreMaxDays(t *testing.T) {
	env := newTestEnv(t, "")
	env.proxy.cfg.MaxDays = 3
	status, _, _ := env.do(t, "GET", "/select/logsql/query?query=*&start=2026-01-01T00:00:00Z&end=2026-11-04T12:00:00Z", "", "")
	if status != 200 {
		t.Fatalf("unexpected status %d", status)
	}
	got := env.ensurer.take()
	want := [][]string{{"20261102", "20261103", "20261104"}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("unexpected EnsureDays calls %v; want %v", got, want)
	}
}

func TestProxyStreamsBigResponse(t *testing.T) {
	env := newTestEnv(t, "")
	status, _, body := env.do(t, "GET", "/big", "", "")
	if status != 200 {
		t.Fatalf("unexpected status %d", status)
	}
	if n := strings.Count(body, "\n"); n != 1000 {
		t.Fatalf("unexpected number of lines: %d", n)
	}
	waitFor(t, func() bool { return env.proxy.gate.inFlight() == 0 })
}

func TestProxyTailDoesNotBlockGate(t *testing.T) {
	env := newTestEnv(t, "/admin")
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI("http://proxy/admin/select/logsql/tail?query=*")
	if err := env.client.Do(req, resp); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != 200 {
		t.Fatalf("unexpected status %d", resp.StatusCode())
	}
	// The stream is open, but it doesn't count as an in-flight request.
	if n := env.proxy.gate.inFlight(); n != 0 {
		t.Fatalf("tail must not hold the gate; in-flight=%d", n)
	}
	br := bufio.NewReader(resp.BodyStream())
	if line, err := br.ReadString('\n'); err != nil || strings.TrimSpace(line) != `{"started":true}` {
		t.Fatalf("unexpected first line %q, %v", line, err)
	}
	for i := range 3 {
		want := fmt.Sprintf(`{"n":%d}`, i)
		env.tailCh <- want
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("cannot read streamed line: %s", err)
		}
		if strings.TrimSpace(line) != want {
			t.Fatalf("unexpected line %q; want %q", line, want)
		}
	}
	close(env.tailCh)
	if _, err := io.ReadAll(br); err != nil {
		t.Fatalf("unexpected error at the end of the stream: %s", err)
	}
}

func TestProxyBackendUnavailable(t *testing.T) {
	env := newTestEnv(t, "")
	be, err := newBackend("http://vl:9428", "")
	if err != nil {
		t.Fatal(err)
	}
	be.hc.Dial = func(string) (net.Conn, error) { return nil, errors.New("connection refused") }
	env.proxy.setBackend(be)
	status, _, body := env.do(t, "GET", "/select/logsql/query?query=*", "", "")
	if status != fasthttp.StatusBadGateway {
		t.Fatalf("unexpected status %d; want 502: %s", status, body)
	}
	if n := env.proxy.gate.inFlight(); n != 0 {
		t.Fatalf("unexpected in-flight requests: %d", n)
	}
}

func TestProxyWaitsForRestart(t *testing.T) {
	env := newTestEnv(t, "")
	env.proxy.gate.close(time.Second)
	done := make(chan int)
	go func() {
		status, _, _ := env.do(t, "GET", "/select/logsql/query?query=*", "", "")
		done <- status
	}()
	select {
	case <-done:
		t.Fatal("requests must wait while VictoriaLogs is restarting")
	case <-time.After(100 * time.Millisecond):
	}
	env.proxy.gate.open()
	if status := <-done; status != 200 {
		t.Fatalf("unexpected status %d", status)
	}
}

// fakeRestarter emulates a VictoriaLogs restart: all partition directories become attached.
type fakeRestarter struct {
	env      *testEnv
	calls    atomic.Int64
	err      error
	gateSeen atomic.Bool
}

func (r *fakeRestarter) Restart(_ context.Context, _ string) error {
	r.calls.Add(1)
	r.env.proxy.gate.mu.Lock()
	r.gateSeen.Store(r.env.proxy.gate.closed)
	r.env.proxy.gate.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.env.mu.Lock()
	r.env.attached = append(r.env.attached, "20261104")
	r.env.mu.Unlock()
	return nil
}

func TestPartitionLoaderAttach(t *testing.T) {
	env := newTestEnv(t, "")
	restarter := &fakeRestarter{env: env}
	l := &partitionLoader{proxy: env.proxy, restarter: restarter, drainTimeout: time.Second}
	if err := l.Load(context.Background(), "20261104"); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if restarter.calls.Load() != 0 {
		t.Fatalf("restart must not be called when attach succeeds")
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	if !slices.Equal(env.attached, []string{"20261104"}) {
		t.Fatalf("unexpected attached partitions: %v", env.attached)
	}
}

func TestPartitionLoaderRestartFallback(t *testing.T) {
	env := newTestEnv(t, "")
	env.failAtt.Store(true)
	restarter := &fakeRestarter{env: env}
	l := &partitionLoader{proxy: env.proxy, restarter: restarter, drainTimeout: time.Second}
	if err := l.Load(context.Background(), "20261104"); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if restarter.calls.Load() != 1 {
		t.Fatalf("restart must be called once when attach fails; got %d", restarter.calls.Load())
	}
	if !restarter.gateSeen.Load() {
		t.Fatalf("new requests must be blocked during restart")
	}
	if n := env.proxy.gate.inFlight(); n != 0 {
		t.Fatalf("unexpected in-flight requests: %d", n)
	}
	// The gate is open again.
	if status, _, _ := env.do(t, "GET", "/select/logsql/query?query=*", "", ""); status != 200 {
		t.Fatalf("unexpected status %d", status)
	}

	// Restart failure is reported.
	restarter.err = errors.New("cannot start")
	if err := l.Load(context.Background(), "20261103"); err == nil {
		t.Fatalf("expecting error")
	}
}

func TestPartitionLoaderExternalMode(t *testing.T) {
	env := newTestEnv(t, "")
	env.failAtt.Store(true)
	l := &partitionLoader{proxy: env.proxy, drainTimeout: time.Second}
	if err := l.Load(context.Background(), "20261104"); err == nil {
		t.Fatalf("expecting error when attach fails without a restarter")
	}
	// Already attached partitions are fine.
	env.mu.Lock()
	env.attached = append(env.attached, "20261103")
	env.mu.Unlock()
	if err := l.Load(context.Background(), "20261103"); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestProxyCloseStreamsInterruptsTail(t *testing.T) {
	env := newTestEnv(t, "")
	defer close(env.tailCh)
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI("http://proxy/select/logsql/tail?query=*")
	if err := env.client.Do(req, resp); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.BodyStream())
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("cannot read the first line: %s", err)
	}
	// Wait until the stream is registered.
	waitFor(t, func() bool {
		env.proxy.streamsMu.Lock()
		defer env.proxy.streamsMu.Unlock()
		return len(env.proxy.streams) == 1
	})
	if n := env.proxy.closeStreams(true); n != 1 {
		t.Fatalf("expecting 1 interrupted stream; got %d", n)
	}
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(br)
		done <- err
	}()
	select {
	case <-done:
		// The client sees the end of the stream (an error or EOF, depending on timing).
	case <-time.After(5 * time.Second):
		t.Fatal("the client stream must end after closeStreams")
	}
	waitFor(t, func() bool {
		env.proxy.streamsMu.Lock()
		defer env.proxy.streamsMu.Unlock()
		return len(env.proxy.streams) == 0
	})
	if n := env.proxy.gate.inFlight(); n != 0 {
		t.Fatalf("unexpected in-flight requests: %d", n)
	}
}

func TestProxyGetWithBodyDoesNotHang(t *testing.T) {
	env := newTestEnv(t, "")
	for _, method := range []string{"GET", "HEAD"} {
		// fasthttp clients refuse to send GET bodies, so write the request by hand.
		conn, err := env.feLn.Dial()
		if err != nil {
			t.Fatal(err)
		}
		raw := method + " /select/logsql/query?query=* HTTP/1.1\r\nHost: proxy\r\nContent-Length: 11\r\nConnection: close\r\n\r\nhello world"
		if _, err := conn.Write([]byte(raw)); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp := fasthttp.AcquireResponse()
		resp.SkipBody = method == "HEAD"
		err = resp.Read(bufio.NewReader(conn))
		_ = conn.Close()
		if err != nil {
			t.Fatalf("%s with body: cannot read response (the backend request probably hangs): %s", method, err)
		}
		if resp.StatusCode() != 200 {
			t.Fatalf("%s with body: unexpected status %d: %s", method, resp.StatusCode(), resp.Body())
		}
		fasthttp.ReleaseResponse(resp)
		last := env.lastReq.Load()
		if v := last.Header.Peek(fasthttp.HeaderContentLength); len(v) > 0 || len(last.Body()) > 0 {
			t.Fatalf("%s: Content-Length and body must not be forwarded; got Content-Length=%q, body=%q", method, v, last.Body())
		}
		waitFor(t, func() bool { return env.proxy.gate.inFlight() == 0 })
	}
}

func TestProxyInsufficientDiskSpace(t *testing.T) {
	env := newTestEnv(t, "")
	env.ensurer.err = fmt.Errorf("cannot restore partition 20261104: %w", ErrInsufficientDiskSpace)
	status, _, body := env.do(t, "GET", "/select/logsql/query?query=*&start=2026-11-04T00:00:00Z&end=2026-11-04T01:00:00Z", "", "")
	if status != fasthttp.StatusInsufficientStorage {
		t.Fatalf("unexpected status %d; want 507: %s", status, body)
	}
	if !strings.Contains(body, "insufficient disk space") {
		t.Fatalf("unexpected response body: %s", body)
	}
	if env.lastReq.Load() != nil {
		t.Fatalf("the request must not be forwarded to VictoriaLogs")
	}
	// Requests which don't trigger a restore are still served.
	if status, _, body := env.do(t, "GET", "/health", "", ""); status != 200 {
		t.Fatalf("unexpected status %d: %s", status, body)
	}
}
