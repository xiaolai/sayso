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
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/xiaolai/sayso/go/internal/wire"

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
	// stderrDrain is how long Close waits, once the child is gone, for its last stderr bytes.
	stderrDrain = 100 * time.Millisecond
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

// String names the executable only: Env holds the inherited environment, which may hold credentials.
func (cfg Config) String() string { return "codexapp.Config{" + cfg.Bin + "}" }

// GoString redacts like String.
func (cfg Config) GoString() string { return cfg.String() }

// Format redacts like String for every verb.
func (cfg Config) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(cfg.String())) }

// RPCError is a JSON-RPC error reply. Message is scrubbed and kept out of Error(), because a misbehaving child may
// put a credential there.
type RPCError struct {
	Code    int
	Message string
}

// CodeMethodNotFound is the JSON-RPC code for an unknown method.
const CodeMethodNotFound = -32601

func (e *RPCError) Error() string { return fmt.Sprintf("JSON-RPC error %d", e.Code) }

// Client is one app-server child, alive until Close. A Client dropped without Close is closed when it is collected:
// the readers hold its core, never the Client. Its methods keep it alive while they run, so the cleanup cannot close
// a child in use (an embedded core would let a method value outlive the Client).
type Client struct{ k *core }

// UserAgent returns the userAgent of the initialize result, "" when absent.
func (c *Client) UserAgent() string { defer runtime.KeepAlive(c); return c.k.agent }

// PID returns the child's process ID.
func (c *Client) PID() int { defer runtime.KeepAlive(c); return c.k.cmd.Process.Pid }

// Stderr returns the first 64 KiB the child wrote to stderr, scrubbed. It is for diagnostics a caller chooses to
// show; the package never logs it, because the scrubber cannot recognise every credential format.
func (c *Client) Stderr() string {
	defer runtime.KeepAlive(c)
	return secret.Scrub(c.k.stderr.String())
}

// Call sends one request and decodes the result into out (when non-nil). It fails with a codex-error after the
// per-call timeout, on a crash or on malformed output; a JSON-RPC error reply is a codex-error wrapping *RPCError.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	defer runtime.KeepAlive(c)
	return c.k.call(ctx, method, params, out)
}

// Close ends the child: stdin closed, SIGTERM to its process group, SIGKILL to the group after 2 s when the child
// or any other member of its group is still alive. It returns within grace + 1 s even when a descendant outside the
// group still holds the pipes: our read ends are closed and the readers abandoned. Safe to call more than once.
func (c *Client) Close() error { defer runtime.KeepAlive(c); return c.k.close() }

// core is the child, its pipes, its readers and the pending calls.
type core struct {
	cmd     *exec.Cmd
	stdin   *os.File // our write end, pollable, so every write has a deadline and Close interrupts it
	stdout  *os.File // our read ends; closed by Close so abandoned readers end
	stderrR *os.File
	timeout time.Duration
	logger  *slog.Logger
	stderr  boundedBuffer
	agent   string // initialize userAgent, scrubbed

	// writeSem admits one writer at a time; a channel, so a waiting writer observes its deadline and cancellation.
	writeSem   chan struct{}
	stderrDone chan struct{} // closed when the stderr copy ends
	mu         sync.Mutex
	nextID     int64
	pending    map[string]chan response
	failure    error         // set once the child is unusable
	dead       chan struct{} // closed when the reader stops
	exited     chan struct{} // closed when the child is reaped
	closeMu    sync.Once
}

type response struct {
	result    json.RawMessage
	hasResult bool // the reply had a result member, null included
	err       error
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
	k := &core{
		timeout:    cfg.Timeout,
		logger:     cfg.Logger,
		pending:    map[string]chan response{},
		dead:       make(chan struct{}),
		exited:     make(chan struct{}),
		writeSem:   make(chan struct{}, 1),
		stderrDone: make(chan struct{}),
	}
	c := &Client{k: k}
	if k.logger == nil {
		k.logger = slog.New(slog.DiscardHandler)
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
	inR, inW, err := os.Pipe()
	if err != nil {
		closeAll(outR, outW, errR, errW)
		return nil, codexError("Codex app-server could not be started", err)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	if err := cmd.Start(); err != nil {
		closeAll(inR, inW, outR, outW, errR, errW)
		return nil, codexError("Codex app-server could not be started", err)
	}
	closeAll(inR, outW, errW) // the child holds its copies
	k.cmd, k.stdin, k.stdout, k.stderrR = cmd, inW, outR, errR

	// The goroutines hold the core only, so an unclosed Client can be collected, and its cleanup ends the child.
	go k.readLoop(outR)
	go func() {
		_, _ = io.Copy(&k.stderr, errR)
		close(k.stderrDone)
	}()
	go func() {
		_ = cmd.Wait()
		close(k.exited)
	}()
	runtime.AddCleanup(c, func(k *core) { _ = k.close() }, k)

	name := cfg.ClientName
	if name == "" {
		name = "sayso"
	}
	init := map[string]any{
		"clientInfo":   map[string]any{"name": name, "version": "0"},
		"capabilities": map[string]any{},
	}
	var hello json.RawMessage
	if err := k.call(ctx, "initialize", init, &hello); err != nil {
		_ = c.Close()
		return nil, err
	}
	agent, err := field[string](hello, "userAgent")
	if err != nil {
		_ = c.Close()
		return nil, codexError("Codex app-server sent an unexpected initialize result", nil)
	}
	k.agent = secret.Scrub(agent)
	if err := k.write(ctx, map[string]any{"method": "initialized"}, time.Now().Add(k.timeout)); err != nil {
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

// readLoop matches replies to pending calls until the child's stdout ends or misbehaves. A server request is
// answered with an error and never acted on.
func (k *core) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine+1) // a line of exactly maxLine bytes fits with its newline
	var failure error
	for sc.Scan() {
		if !k.handle(trimJSONSpace(sc.Bytes())) {
			// The line is never quoted: it may carry a credential.
			failure = codexError("Codex app-server sent malformed JSON", nil)
			break
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
	k.fail(failure)
	_, _ = io.Copy(io.Discard, r) // keep the child from blocking on a full pipe
}

// trimJSONSpace removes JSON's own whitespace (space, tab, line feed, carriage return) from both ends, and nothing
// else: any other space is not JSON and makes the line malformed.
func trimJSONSpace(b []byte) []byte {
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	for len(b) > 0 && isSpace(b[0]) {
		b = b[1:]
	}
	for len(b) > 0 && isSpace(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return b
}

// handle reads one line (spec/credentials.md, messages); false when it is not JSON-RPC. A message is classified
// before its reply fields are read: a notification (no id) is ignored whatever else it holds, and a message with a
// method, even "", is a server request. Keys are exact.
func (k *core) handle(line []byte) bool {
	if len(line) == 0 {
		return true
	}
	if !ValidJSON(line) {
		return false
	}
	root, err := wire.Parse(line)
	if err != nil {
		return false
	}
	if root.IsNull() {
		return true
	}
	m, err := wire.AsObject(root)
	if err != nil {
		return false
	}
	_, hasMethod, err := m.String("method")
	if err != nil {
		return false
	}
	id, hasID := m.Field("id")
	switch {
	case !hasID:
		return true // a notification
	case hasMethod:
		k.refuse(id)
		return true
	}
	var resp response
	if e, ok, err := m.Object("error"); err != nil {
		return false
	} else if ok {
		code, _, err := e.Int("code")
		if err != nil {
			return false
		}
		msg, _, err := e.String("message")
		if err != nil {
			return false
		}
		resp.err = &RPCError{Code: int(code), Message: secret.Scrub(msg)}
	} else if r, ok := m["result"]; ok {
		resp.result, resp.hasResult = json.RawMessage(r.Raw()), true
	}
	if id.Kind() != '0' {
		return true // not one of ours
	}
	n, err := strconv.ParseInt(id.Raw(), 10, 64)
	if err != nil {
		return true // a fraction or an exponent: not one of ours
	}
	k.deliver(strconv.FormatInt(n, 10), resp)
	return true
}

// refuse answers a server request (an approval, for instance) with an error.
func (k *core) refuse(id wire.Node) {
	msg := map[string]any{"id": json.RawMessage(id.Raw()), "error": map[string]any{"code": CodeMethodNotFound, "message": "server requests are not supported"}}
	_ = k.write(context.Background(), msg, time.Now().Add(k.timeout))
}

func (k *core) deliver(id string, resp response) {
	k.mu.Lock()
	ch, ok := k.pending[id]
	delete(k.pending, id)
	k.mu.Unlock()
	if ok {
		ch <- resp // buffered, never blocks
	}
}

// fail marks the client unusable and releases every pending call.
func (k *core) fail(err error) {
	k.mu.Lock()
	if k.failure == nil {
		k.failure = err
		close(k.dead)
	}
	k.mu.Unlock()
}

func (k *core) failed() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.failure
}

// write sends one message, giving up at deadline and when ctx ends: a child that stops reading cannot hold a call,
// or the reader that refuses a server request, past its timeout. Waiting for another writer counts against the
// deadline too. A write cut off after part of the message was sent leaves the stream unusable, so the client is
// failed. Close interrupts a write in progress.
func (k *core) write(ctx context.Context, v any, deadline time.Time) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("codexapp: encode: %w", err)
	}
	b = append(b, '\n')
	timedOut := codexError("Codex app-server did not accept a request within "+k.timeout.String(), nil)
	if err := ctx.Err(); err != nil {
		return err
	}
	wait := time.NewTimer(time.Until(deadline))
	defer wait.Stop()
	select {
	case k.writeSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-k.dead:
		return k.failed()
	case <-wait.C:
		return timedOut
	}
	defer func() { <-k.writeSem }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := k.failed(); err != nil {
		return err
	}
	if !time.Now().Before(deadline) {
		return timedOut
	}
	if err := k.stdin.SetWriteDeadline(deadline); err != nil {
		return codexError("Codex app-server is not accepting requests", nil)
	}
	// Cancellation interrupts the write by moving its deadline; active keeps a late callback off the next write.
	var mu sync.Mutex
	active := true
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		if active {
			_ = k.stdin.SetWriteDeadline(time.Now())
		}
	})
	defer func() {
		stop()
		mu.Lock()
		active = false
		mu.Unlock()
	}()
	n, err := k.stdin.Write(b)
	if err == nil {
		return nil
	}
	if n > 0 {
		k.fail(codexError("Codex app-server stream was cut off inside a request", nil))
	}
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, os.ErrDeadlineExceeded):
		return timedOut
	}
	return codexError("Codex app-server is not accepting requests", nil)
}

func (k *core) call(ctx context.Context, method string, params, out any) error {
	if err := ctx.Err(); err != nil {
		return err // an already-cancelled call sends nothing
	}
	ctx, cancel := context.WithTimeout(ctx, k.timeout)
	defer cancel()
	k.mu.Lock()
	if k.failure != nil {
		err := k.failure
		k.mu.Unlock()
		return err
	}
	k.nextID++
	id := k.nextID
	key := strconv.FormatInt(id, 10)
	ch := make(chan response, 1)
	k.pending[key] = ch
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		delete(k.pending, key)
		k.mu.Unlock()
	}()

	if params == nil {
		params = map[string]any{}
	}
	deadline, _ := ctx.Deadline() // set: ctx carries the per-call timeout
	if err := k.write(ctx, map[string]any{"id": id, "method": method, "params": params}, deadline); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return codexError("Codex app-server did not accept "+method+" within "+k.timeout.String(), nil)
		}
		return err
	}
	answer := func(resp response) error {
		if resp.err != nil {
			return codexError("Codex app-server rejected "+method, resp.err)
		}
		if !resp.hasResult { // neither result nor error, whether or not the caller reads the result
			return codexError("Codex app-server sent an unexpected "+method+" result", nil)
		}
		if out != nil {
			if err := json.Unmarshal(resp.result, out); err != nil {
				return codexError("Codex app-server sent an unexpected "+method+" result", nil)
			}
		}
		return nil
	}
	select {
	case resp := <-ch:
		return answer(resp)
	case <-k.dead:
		select {
		case resp := <-ch: // delivered before the stream ended: the answer stands
			return answer(resp)
		default:
		}
		return k.failed()
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return codexError(fmt.Sprintf("Codex app-server did not answer %s within %s", method, k.timeout), nil)
		}
		return ctx.Err()
	}
}

func (k *core) close() error {
	k.closeMu.Do(func() {
		k.fail(codexError("Codex app-server was closed", nil))
		_ = k.stdin.Close()
		pid := k.cmd.Process.Pid
		// Once the child is reaped and its group is empty, the group ID may already name another process's group, so
		// nothing is signalled.
		if !k.gone(pid) {
			_ = syscall.Kill(-pid, syscall.SIGTERM)
		}
		if !k.waitGroup(pid, time.Now().Add(killGrace)) {
			_ = syscall.Kill(-pid, syscall.SIGKILL) // the group was alive a moment ago, so its ID is still ours
			select {
			case <-k.exited:
			case <-time.After(reapGrace):
				k.logger.Debug("codex app-server was not reaped within the grace; abandoning it", "pid", pid)
			}
		}
		// The last stderr bytes the child wrote are kept, unless a descendant still holds the pipe.
		select {
		case <-k.stderrDone:
		case <-time.After(stderrDrain):
		}
		closeAll(k.stdout, k.stderrR)
		if n := k.stderr.Len(); n > 0 {
			k.logger.Debug("codex app-server wrote to stderr", "bytes", n)
		}
	})
	return nil
}

// gone reports whether the child was reaped and no member of its process group remains.
func (k *core) gone(pid int) bool {
	select {
	case <-k.exited:
		return !groupAlive(pid)
	default:
		return false
	}
}

// waitGroup reports whether the child was reaped and no other member of its process group remains before deadline.
// A group outlives its leader while a member lives, so the group ID still names our group while kill(-pid, 0)
// succeeds.
func (k *core) waitGroup(pid int, deadline time.Time) bool {
	for {
		if k.gone(pid) {
			return true
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		pause := time.NewTimer(min(left, 20*time.Millisecond))
		select {
		case <-k.exited:
			// Reaped, but a member may still hold the group: the probes that follow are paced, not spun.
			<-pause.C
		case <-pause.C:
		}
	}
}

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
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
