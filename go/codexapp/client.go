// Package codexapp runs `codex app-server --listen stdio://` as a short-lived child and speaks its newline-delimited
// JSON-RPC (spec/credentials.md, the app-server source).
package codexapp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/secret"
)

const (
	// killGrace is how long Close waits after SIGTERM before SIGKILL.
	killGrace = 2 * time.Second
	// reapGrace is how long Close waits after SIGKILL before abandoning the child.
	reapGrace = time.Second
	// stderrCap bounds the retained child stderr.
	stderrCap = 64 << 10
	// maxLine bounds one JSON-RPC message from the child.
	maxLine = 4 << 20
)

// Args are the child's arguments. The provider override makes Codex answer for its OpenAI sign-in even when the
// user's configured provider needs none.
var Args = []string{"-c", `model_provider="openai"`, "app-server", "--listen", "stdio://"}

// Config describes how to start the child.
type Config struct {
	Bin       string        // absolute path, from Resolve
	CodexHome string        // passed as CODEX_HOME when non-empty
	Env       []string      // the inherited environment; nil means os.Environ()
	Timeout   time.Duration // per call; required
	Logger    *slog.Logger  // nil discards
	// ClientName is sent as clientInfo.name; "" means "sayso".
	ClientName string
	// Guard, if set, runs before the executable is started; an error refuses the start. A consumer uses it to keep
	// unit tests away from a real Codex.
	Guard func(bin string) error
}

// RPCError is a JSON-RPC error reply. Message is scrubbed and kept out of Error(), because a misbehaving child may
// put a credential there.
type RPCError struct {
	Code    int
	Message string
}

// CodeMethodNotFound is the JSON-RPC code for an unknown method.
const CodeMethodNotFound = -32601

func (e *RPCError) Error() string { return fmt.Sprintf("JSON-RPC error %d", e.Code) }

// Client is one app-server child, alive until Close.
type Client struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *os.File // our read ends; closed by Close so abandoned readers end
	stderrR *os.File
	timeout time.Duration
	logger  *slog.Logger
	stderr  boundedBuffer
	agent   string // initialize userAgent, scrubbed

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[string]chan response
	failure error         // set once the child is unusable
	dead    chan struct{} // closed when the reader stops
	exited  chan struct{} // closed when the child is reaped
	closeMu sync.Once
}

type response struct {
	result json.RawMessage
	err    error
}

type wireMsg struct {
	ID     json.RawMessage `json:"id"` // a number for our requests; any JSON value for a server request
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// codexError builds a codex-error failure; cause is scrubbed by openaihttp.Error.
func codexError(msg string, cause error) error {
	return &openaihttp.Error{Class: openaihttp.ClassCodexError, Message: msg, Err: cause}
}

// Start spawns the child in its own process group and completes the initialize handshake.
func Start(ctx context.Context, cfg Config) (*Client, error) {
	if !filepath.IsAbs(cfg.Bin) {
		return nil, fmt.Errorf("codexapp: executable path %q is not absolute", cfg.Bin)
	}
	if cfg.Timeout <= 0 {
		return nil, errors.New("codexapp: a per-call Timeout is required")
	}
	if cfg.Guard != nil {
		if err := cfg.Guard(cfg.Bin); err != nil {
			return nil, fmt.Errorf("codexapp: start refused: %w", err)
		}
	}
	env := cfg.Env
	if env == nil {
		env = os.Environ()
	}
	c := &Client{
		timeout: cfg.Timeout,
		logger:  cfg.Logger,
		pending: map[string]chan response{},
		dead:    make(chan struct{}),
		exited:  make(chan struct{}),
	}
	if c.logger == nil {
		c.logger = slog.New(slog.DiscardHandler)
	}
	// No CommandContext: the child must outlive ctx and is reaped only by Close.
	cmd := exec.Command(cfg.Bin, Args...)
	cmd.Env = ChildEnv(env, cfg.Bin, cfg.CodexHome)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Our own pipes, handed to the child as files: Wait then waits for the process only, never for a descendant
	// that inherited the write ends, and Close can end our readers by closing the read ends.
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, codexError("Codex app-server could not be started", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(outR, outW)
		return nil, codexError("Codex app-server could not be started", err)
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	stdin, err := cmd.StdinPipe()
	if err != nil {
		closeAll(outR, outW, errR, errW)
		return nil, codexError("Codex app-server could not be started", err)
	}
	if err := cmd.Start(); err != nil {
		closeAll(outR, outW, errR, errW)
		return nil, codexError("Codex app-server could not be started", err)
	}
	closeAll(outW, errW) // the child holds its copies
	c.cmd, c.stdin, c.stdout, c.stderrR = cmd, stdin, outR, errR

	go c.readLoop(outR)
	go func() { _, _ = io.Copy(&c.stderr, errR) }()
	go func() {
		_ = cmd.Wait()
		close(c.exited)
	}()

	name := cfg.ClientName
	if name == "" {
		name = "sayso"
	}
	init := map[string]any{
		"clientInfo":   map[string]any{"name": name, "version": "0"},
		"capabilities": map[string]any{},
	}
	var hello struct {
		UserAgent string `json:"userAgent"`
	}
	if err := c.Call(ctx, "initialize", init, &hello); err != nil {
		_ = c.Close()
		return nil, err
	}
	c.agent = secret.Scrub(hello.UserAgent)
	if err := c.write(map[string]any{"method": "initialized"}); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// UserAgent returns the userAgent of the initialize result, "" when absent.
func (c *Client) UserAgent() string { return c.agent }

// PID returns the child's process ID.
func (c *Client) PID() int { return c.cmd.Process.Pid }

// Stderr returns the first 64 KiB the child wrote to stderr, scrubbed. It is for diagnostics a caller chooses to
// show; the package never logs it, because the scrubber cannot recognise every credential format.
func (c *Client) Stderr() string { return secret.Scrub(c.stderr.String()) }

// readLoop matches replies to pending calls until the child's stdout ends or misbehaves. A server request is
// answered with an error and never acted on.
func (c *Client) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine+1) // a line of exactly maxLine bytes fits with its newline
	var failure error
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m wireMsg
		if err := json.Unmarshal(line, &m); err != nil {
			// The line is never quoted: it may carry a credential.
			failure = codexError("Codex app-server sent malformed JSON", nil)
			break
		}
		switch {
		case len(m.ID) == 0 || string(m.ID) == "null":
			// a notification
		case m.Method != "":
			c.refuse(m.ID)
		default:
			c.deliver(string(m.ID), m)
		}
	}
	if failure == nil {
		if err := sc.Err(); errors.Is(err, bufio.ErrTooLong) {
			failure = codexError("Codex app-server sent a line longer than 4 MiB", nil)
		} else if err != nil {
			failure = codexError("Codex app-server output could not be read", nil)
		} else {
			failure = codexError("Codex app-server exited unexpectedly", nil)
		}
	}
	c.fail(failure)
	_, _ = io.Copy(io.Discard, r) // keep the child from blocking on a full pipe
}

// refuse answers a server request (an approval, for instance) with an error.
func (c *Client) refuse(id json.RawMessage) {
	_ = c.write(map[string]any{"id": id, "error": map[string]any{"code": CodeMethodNotFound, "message": "server requests are not supported"}})
}

func (c *Client) deliver(id string, m wireMsg) {
	c.mu.Lock()
	ch, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if !ok {
		return
	}
	var resp response
	if m.Error != nil {
		resp.err = &RPCError{Code: m.Error.Code, Message: secret.Scrub(m.Error.Message)}
	} else {
		resp.result = m.Result
	}
	ch <- resp // buffered, never blocks
}

// fail marks the client unusable and releases every pending call.
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.failure == nil {
		c.failure = err
		close(c.dead)
	}
	c.mu.Unlock()
}

func (c *Client) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("codexapp: encode: %w", err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(append(b, '\n')); err != nil {
		return codexError("Codex app-server is not accepting requests", nil)
	}
	return nil
}

// Call sends one request and decodes the result into out (when non-nil). It fails with a codex-error after the
// per-call timeout, on a crash or on malformed output; a JSON-RPC error reply is a codex-error wrapping *RPCError.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	c.mu.Lock()
	if c.failure != nil {
		err := c.failure
		c.mu.Unlock()
		return err
	}
	c.nextID++
	id := c.nextID
	key := strconv.FormatInt(id, 10)
	ch := make(chan response, 1)
	c.pending[key] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()

	if params == nil {
		params = map[string]any{}
	}
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case resp := <-ch:
		if resp.err != nil {
			return codexError("Codex app-server rejected "+method, resp.err)
		}
		if out != nil {
			if err := json.Unmarshal(resp.result, out); err != nil {
				return codexError("Codex app-server sent an unexpected "+method+" result", nil)
			}
		}
		return nil
	case <-c.dead:
		c.mu.Lock()
		err := c.failure
		c.mu.Unlock()
		return err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return codexError(fmt.Sprintf("Codex app-server did not answer %s within %s", method, c.timeout), nil)
		}
		return ctx.Err()
	}
}

// Close ends the child: stdin closed, SIGTERM to its process group, SIGKILL to the group after 2 s. It returns within
// grace + 1 s even when a descendant outside the group still holds the pipes: our read ends are closed and the
// readers abandoned. Safe to call more than once.
func (c *Client) Close() error {
	c.closeMu.Do(func() {
		_ = c.stdin.Close()
		pgid := -c.cmd.Process.Pid
		_ = syscall.Kill(pgid, syscall.SIGTERM)
		select {
		case <-c.exited:
		case <-time.After(killGrace):
			_ = syscall.Kill(pgid, syscall.SIGKILL)
			select {
			case <-c.exited:
			case <-time.After(reapGrace):
				c.logger.Debug("codex app-server was not reaped within the grace; abandoning it", "pid", c.cmd.Process.Pid)
			}
		}
		closeAll(c.stdout, c.stderrR)
		if n := c.stderr.Len(); n > 0 {
			c.logger.Debug("codex app-server wrote to stderr", "bytes", n)
		}
	})
	return nil
}

// boundedBuffer keeps the first stderrCap bytes and discards the rest.
type boundedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := stderrCap - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *boundedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}
