// Package supervisor runs a VictoriaLogs child process on a free local port,
// waits until it is healthy, restarts it when it exits unexpectedly and stops it
// on shutdown.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/vlclient"
)

// MaxRetention is the -retentionPeriod passed to VictoriaLogs.
//
// VictoriaLogs rejects retention periods bigger than 1200 months of 31 days
// (see lib/flagutil.RetentionDuration); 100y is the largest round value below that limit.
const MaxRetention = "100y"

// Config configures a Supervisor.
type Config struct {
	// Bin is the path to the VictoriaLogs binary.
	Bin string
	// StorageDir is passed as -storageDataPath.
	StorageDir string
	// AuthKey is passed as -partitionManageAuthKey when non-empty.
	AuthKey string
	// ListenHost is the host for -httpListenAddr. Defaults to 127.0.0.1.
	ListenHost string
	// HealthTimeout is the maximum time to wait until a started process becomes healthy.
	HealthTimeout time.Duration
	// StopTimeout is the time to wait after SIGTERM before killing the process.
	StopTimeout time.Duration
	// Output receives stdout and stderr of the child process. Defaults to os.Stderr.
	Output io.Writer
	// DropEnv lists environment variables which must not be passed to the child process.
	DropEnv []string
	// OnReady is called with the base URL (http://host:port) each time a new
	// child process becomes healthy. It is called while the supervisor lock is held,
	// so it must not call Supervisor methods.
	OnReady func(baseURL string)
}

// Supervisor manages a single VictoriaLogs child process.
type Supervisor struct {
	cfg Config

	// ctx is canceled by Stop in order to interrupt pending starts.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	cur     *child
	gen     uint64
	stopped bool
	// autoRestarting is true while a background restart loop is running.
	autoRestarting bool
}

type child struct {
	cmd     *exec.Cmd
	baseURL string
	done    chan struct{}
	waitErr error
	// intentional is set before the process is stopped on purpose.
	intentional bool
}

// New creates a supervisor. Call Start to launch the process.
func New(cfg Config) *Supervisor {
	if cfg.ListenHost == "" {
		cfg.ListenHost = "127.0.0.1"
	}
	if cfg.HealthTimeout <= 0 {
		cfg.HealthTimeout = 3 * time.Minute
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = 30 * time.Second
	}
	if cfg.Output == nil {
		cfg.Output = os.Stderr
	}
	if cfg.OnReady == nil {
		cfg.OnReady = func(string) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Supervisor{cfg: cfg, ctx: ctx, cancel: cancel}
}

// Start starts the child process and waits until it is healthy.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return errors.New("supervisor is stopped")
	}
	if s.cur != nil {
		return errors.New("VictoriaLogs is already running")
	}
	return s.startLocked(ctx)
}

// Restart stops the running child process (if any), starts a new one and
// waits until it is healthy. If the new process cannot be started, background
// restart attempts with backoff are scheduled and the error is returned.
func (s *Supervisor) Restart(ctx context.Context, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return errors.New("supervisor is stopped")
	}
	slog.Info("restarting VictoriaLogs", "reason", reason)
	if s.cur != nil {
		s.stopLocked(s.cur)
		s.cur = nil
	}
	err := s.startLocked(ctx)
	if err != nil {
		s.scheduleAutoRestartLocked()
	}
	return err
}

// Stop stops the child process and disables automatic restarts.
func (s *Supervisor) Stop() {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	if s.cur != nil {
		s.stopLocked(s.cur)
		s.cur = nil
	}
}

// BaseURL returns the base URL of the running child process or an empty string.
func (s *Supervisor) BaseURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return ""
	}
	return s.cur.baseURL
}

// Args returns the command-line args for a child process listening on listenAddr.
func (s *Supervisor) Args(listenAddr string) []string {
	args := []string{
		"-storageDataPath=" + s.cfg.StorageDir,
		"-httpListenAddr=" + listenAddr,
		"-retentionPeriod=" + MaxRetention,
		"-loggerFormat=json",
	}
	if s.cfg.AuthKey != "" {
		args = append(args, "-partitionManageAuthKey="+s.cfg.AuthKey)
	}
	return args
}

const startAttempts = 3

func (s *Supervisor) startLocked(ctx context.Context) error {
	var lastErr error
	for attempt := 1; attempt <= startAttempts; attempt++ {
		c, err := s.spawn()
		if err != nil {
			// The binary cannot be executed; retrying won't help.
			return err
		}
		err = s.waitHealthy(ctx, c)
		if err == nil {
			s.cur = c
			s.gen++
			go s.monitor(c, s.gen)
			slog.Info("VictoriaLogs is ready", "addr", c.baseURL, "pid", c.cmd.Process.Pid)
			s.cfg.OnReady(c.baseURL)
			return nil
		}
		s.stopLocked(c)
		lastErr = err
		slog.Error("VictoriaLogs failed to start", "attempt", attempt, "pid", c.cmd.Process.Pid, "error", err)
		if ctx.Err() != nil || s.ctx.Err() != nil {
			break
		}
	}
	return fmt.Errorf("cannot start VictoriaLogs: %w", lastErr)
}

func (s *Supervisor) spawn() (*child, error) {
	port, err := freePort(s.cfg.ListenHost)
	if err != nil {
		return nil, fmt.Errorf("cannot find a free port: %w", err)
	}
	listenAddr := net.JoinHostPort(s.cfg.ListenHost, fmt.Sprint(port))
	args := s.Args(listenAddr)
	cmd := exec.Command(s.cfg.Bin, args...)
	cmd.Stdout = s.cfg.Output
	cmd.Stderr = s.cfg.Output
	cmd.Env = filterEnv(os.Environ(), s.cfg.DropEnv)
	setSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cannot start %q: %w", s.cfg.Bin, err)
	}
	c := &child{
		cmd:     cmd,
		baseURL: "http://" + listenAddr,
		done:    make(chan struct{}),
	}
	go func() {
		c.waitErr = cmd.Wait()
		close(c.done)
	}()
	slog.Info("started VictoriaLogs process", "pid", cmd.Process.Pid, "bin", s.cfg.Bin, "args", redactArgs(args))
	return c, nil
}

func (s *Supervisor) waitHealthy(ctx context.Context, c *child) error {
	vl, err := vlclient.New(c.baseURL, "")
	if err != nil {
		return err
	}
	deadline := time.NewTimer(s.cfg.HealthTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		select {
		case <-c.done:
			return fmt.Errorf("process exited before becoming healthy: %v", exitDescription(c.waitErr))
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ctx.Done():
			return errors.New("supervisor is stopping")
		case <-deadline.C:
			return fmt.Errorf("process did not become healthy in %s; last error: %v", s.cfg.HealthTimeout, lastErr)
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			lastErr = vl.Ping(pctx)
			cancel()
			if lastErr == nil {
				return nil
			}
		}
	}
}

// stopLocked stops c: SIGTERM, then SIGKILL after StopTimeout.
func (s *Supervisor) stopLocked(c *child) {
	c.intentional = true
	select {
	case <-c.done:
		return
	default:
	}
	pid := c.cmd.Process.Pid
	slog.Info("stopping VictoriaLogs process", "pid", pid)
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		slog.Warn("cannot send SIGTERM to VictoriaLogs", "pid", pid, "error", err)
	}
	t := time.NewTimer(s.cfg.StopTimeout)
	defer t.Stop()
	select {
	case <-c.done:
		slog.Info("VictoriaLogs process stopped", "pid", pid)
	case <-t.C:
		slog.Warn("VictoriaLogs did not stop in time; killing it", "pid", pid, "timeout", s.cfg.StopTimeout.String())
		_ = c.cmd.Process.Kill()
		<-c.done
	}
}

// monitor waits for c to exit and restarts it if the exit was not intentional.
func (s *Supervisor) monitor(c *child, gen uint64) {
	<-c.done
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.intentional || s.stopped || s.gen != gen {
		return
	}
	slog.Error("VictoriaLogs exited unexpectedly", "pid", c.cmd.Process.Pid, "exit", exitDescription(c.waitErr))
	s.cur = nil
	s.scheduleAutoRestartLocked()
}

func (s *Supervisor) scheduleAutoRestartLocked() {
	if s.autoRestarting || s.stopped {
		return
	}
	s.autoRestarting = true
	go s.autoRestart()
}

// autoRestart starts the child process with exponential backoff until it is
// running again or the supervisor is stopped.
func (s *Supervisor) autoRestart() {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		select {
		case <-s.ctx.Done():
			s.mu.Lock()
			s.autoRestarting = false
			s.mu.Unlock()
			return
		case <-time.After(backoff):
		}
		s.mu.Lock()
		if s.stopped || s.cur != nil {
			// Stopped or restarted by somebody else.
			s.autoRestarting = false
			s.mu.Unlock()
			return
		}
		err := s.startLocked(s.ctx)
		if err == nil {
			s.autoRestarting = false
			s.mu.Unlock()
			slog.Info("VictoriaLogs has been restarted after unexpected exit")
			return
		}
		s.mu.Unlock()
		slog.Error("cannot restart VictoriaLogs; retrying", "error", err, "retry_in", backoff.String())
		backoff = min(2*backoff, maxBackoff)
	}
}

func freePort(host string) (int, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func filterEnv(env, drop []string) []string {
	if len(drop) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		dropped := false
		for _, d := range drop {
			if name == d {
				dropped = true
				break
			}
		}
		if !dropped {
			out = append(out, kv)
		}
	}
	return out
}

func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "-partitionManageAuthKey=") {
			a = "-partitionManageAuthKey=secret"
		}
		out[i] = a
	}
	return out
}

func exitDescription(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}
