// Package codexapp runs `codex app-server --listen stdio://` as a short-lived child and speaks its newline-delimited
// JSON-RPC (spec/credentials.md, the app-server source).
package codexapp

import (
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
	"weak"

	"github.com/xiaolai/sayso/go/internal/wire"

	"github.com/xiaolai/sayso/go/failure"
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
	// refusalCap bounds the refusals queued for a child that sends server requests without reading the answers.
	refusalCap = 2 * maxLine
	// closeFlush bounds how long Close waits for the messages queued before it, refusals included, to be written
	// before it closes stdin.
	closeFlush = 100 * time.Millisecond
)

// Args are the child's arguments. The provider override makes Codex answer for its OpenAI sign-in even when the
// user's configured provider needs none.
var Args = []string{"-c", `model_provider="openai"`, "app-server", "--listen", "stdio://"}

// Config describes how to start the child.
type Config struct {
	Bin       string        // absolute path, from Resolve
	CodexHome string        // passed as CODEX_HOME when non-empty
	Env       Env           // the child's environment, from EnvOf; the zero Env inherits os.Environ()
	Timeout   time.Duration // per call; required
	Logger    *slog.Logger  // nil discards
	// ClientName is sent as clientInfo.name; "" means "sayso".
	ClientName string
	// Guard, if set, runs before the executable is started; an error refuses the start. A consumer uses it to keep
	// unit tests away from a real Codex.
	Guard func(bin string) error
	// Dir is the child's working directory: an absolute path to an existing folder. "" keeps the caller's.
	Dir string
	// ExperimentalAPI sends capabilities.experimentalApi: true with initialize.
	ExperimentalAPI bool
	// OnEvent, when set, receives every notification and every server request (already refused), in the order the
	// child wrote them, one at a time from a goroutine of its own: the reader never waits for it. A reply may reach
	// its caller before OnEvent has seen the events written before it. Events held beyond 8 MiB (each charged its
	// method and params bytes plus 64) or beyond 65,536 of them, the one being handled included, are a codex-error
	// that fails the client. After a failure the events already queued are still delivered. Once Close has begun no
	// further event is admitted; one already admitted may still be running, and Close does not wait for it, so the
	// handler may call Close or Call itself, on the client it is given. Unset, notifications are ignored.
	//
	// The client is passed rather than captured: the event goroutine holds it only while the handler runs, so an
	// unclosed client is still collected and its child ended. A handler that keeps the client itself keeps it alive.
	OnEvent func(*Client, Event)
}

// String names the executable only: Env may hold credentials.
func (cfg Config) String() string { return "codexapp.Config{" + cfg.Bin + "}" }

// GoString redacts like String.
func (cfg Config) GoString() string { return cfg.String() }

// Format redacts like String for every verb.
func (cfg Config) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(cfg.String())) }

// RPCError is a JSON-RPC error reply. Message is scrubbed and kept out of Error(), because a misbehaving child may
// put a credential there.
type RPCError struct {
	Code    int64 // a signed 64-bit integer, as the wire rules read it, on every platform
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

	// out is every message waiting to be written, requests and answers to server requests alike, in the order they
	// were queued; writeLoop alone writes, so the reader never waits on the child's input and a request is written
	// after every answer queued before it and before every answer queued after. refusalBytes is the size of the
	// answers not yet written (the one being written included), at most refusalCap. Guarded by mu.
	out          []*outbound
	refusalBytes int
	outWake      chan struct{} // one pending wake-up for writeLoop
	writing      bool          // a write to stdin is under way, so fail must interrupt it; guarded by mu
	// events wait for onEvent, delivered in order by eventLoop; eventBytes and eventCount are what they are charged,
	// the one being delivered included, at most eventCap and maxEvents. eventsClosed is set when Close begins, so no
	// event is admitted after it. Guarded by mu.
	onEvent func(*Client, Event)
	// owner is the client the handler is given, held weakly: the event goroutine keeps it only while it delivers.
	owner        weak.Pointer[Client]
	events       []eventData
	eventBytes   int
	eventCount   int
	eventsClosed bool
	eventWake    chan struct{}

	stderrDone chan struct{} // closed when the stderr copy ends
	mu         sync.Mutex
	nextID     int64
	pending    map[string]chan response
	failure    error         // set once the child is unusable
	dead       chan struct{} // closed when the reader stops
	exited     chan struct{} // closed when the child has exited (on macOS and Linux it stays unreaped until Close)
	// lost is set before exited is closed, and read after: the kernel reaped the child itself (the host ignores
	// SIGCHLD), so its ID, and its group ID, may name another process and nothing is signalled.
	lost    bool
	closeMu sync.Once

	// Test seams, nil outside this package's tests: testRefusalAdmitted runs after an answer's slot is admitted and
	// before its bytes are encoded, testEventAdmitted after an event is admitted and before the handler is called.
	testRefusalAdmitted func()
	testEventAdmitted   func()
	testWriterWaiting   func() // runs when writeLoop stops at an answer still being encoded
	// testLiveGroup stands in for the probe of the child's group, testSignal for the signals Close sends it.
	testLiveGroup func(pgid int) bool
	testSignal    func(pgid int, sig syscall.Signal)
}

type response struct {
	result    []byte // a copy of the result member, the call's own, which it clears
	hasResult bool   // the reply had a result member, null included
	err       error
}

// codexError builds a codex-error failure; cause is scrubbed by failure.Error.
func codexError(msg string, cause error) error {
	return &failure.Error{Class: failure.ClassCodexError, Message: msg, Err: cause}
}

// checkDir refuses a working directory that is set but is not an absolute path to an existing folder. The path is
// not quoted: a caller's folder name is not ours to print.
func checkDir(dir string) error {
	if dir == "" {
		return nil
	}
	if st, err := os.Stat(dir); !filepath.IsAbs(dir) || err != nil || !st.IsDir() {
		return errors.New("codexapp: the working directory is not an absolute path to an existing folder")
	}
	return nil
}

// Start spawns the child in its own process group and completes the initialize handshake.
func Start(ctx context.Context, cfg Config) (*Client, error) {
	if !filepath.IsAbs(cfg.Bin) {
		return nil, fmt.Errorf("codexapp: executable path %q is not absolute", cfg.Bin)
	}
	if cfg.Timeout <= 0 {
		return nil, errors.New("codexapp: a per-call Timeout is required")
	}
	if err := checkDir(cfg.Dir); err != nil {
		return nil, err
	}
	if cfg.Guard != nil {
		if err := cfg.Guard(cfg.Bin); err != nil {
			return nil, fmt.Errorf("codexapp: start refused: %w", err)
		}
	}
	env := cfg.Env.List()
	if cfg.Env.IsZero() {
		env = os.Environ()
	}
	k := &core{
		timeout:    cfg.Timeout,
		logger:     cfg.Logger,
		pending:    map[string]chan response{},
		dead:       make(chan struct{}),
		exited:     make(chan struct{}),
		stderrDone: make(chan struct{}),
		outWake:    make(chan struct{}, 1),
		onEvent:    cfg.OnEvent,
		eventWake:  make(chan struct{}, 1),
	}
	c := &Client{k: k}
	k.owner = weak.Make(c)
	if k.logger == nil {
		k.logger = slog.New(slog.DiscardHandler)
	}
	// No CommandContext: the child must outlive ctx and is reaped only by Close.
	cmd := exec.Command(cfg.Bin, Args...)
	cmd.Env = ChildEnv(env, cfg.Bin, cfg.CodexHome)
	cmd.Dir = cfg.Dir
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
	go k.writeLoop()
	if k.onEvent != nil {
		go k.eventLoop()
	}
	go func() {
		_, _ = io.Copy(&k.stderr, errR)
		close(k.stderrDone)
	}()
	go func() {
		k.lost = !waitExit(cmd)
		close(k.exited)
	}()
	// In a goroutine of its own, as Swift's deinit closes on a thread of its own: close may wait out the grace, and
	// the runtime asks a long cleanup not to block the goroutines that run the others.
	runtime.AddCleanup(c, func(k *core) { go func() { _ = k.close() }() }, k)

	name := cfg.ClientName
	if name == "" {
		name = "sayso"
	}
	capabilities := map[string]any{}
	if cfg.ExperimentalAPI {
		capabilities["experimentalApi"] = true
	}
	init := map[string]any{
		"clientInfo":   map[string]any{"name": name, "version": "0"},
		"capabilities": capabilities,
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
	if err := k.send(ctx, map[string]any{"method": "initialized"}, time.Now().Add(k.timeout)); err != nil {
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
// answered with an error and never acted on; it and every notification are queued for OnEvent when one is set.
//
// Every line is read into the reader's one buffer (lineReader) and cleared once it is handled, and the buffer is
// cleared before a failure is recorded, so a call that returns, whatever its outcome, leaves no line of the child's
// behind; what the child writes after a failure is read and cleared.
func (k *core) readLoop(r io.Reader) {
	lines := newLineReader(r)
	var failure error
	for {
		line, err := lines.next()
		if errors.Is(err, errLineTooLong) {
			failure = codexError("Codex app-server sent a line longer than 4 MiB", nil)
			break
		} else if errors.Is(err, io.EOF) {
			failure = codexError("Codex app-server exited unexpectedly", nil)
			break
		} else if err != nil {
			failure = codexError("Codex app-server output could not be read", nil)
			break
		}
		ok := k.handle(trimJSONSpace(line))
		clear(line)
		if !ok {
			// The line is never quoted: it may carry a credential.
			failure = codexError("Codex app-server sent malformed JSON", nil)
			break
		}
		if k.failed() != nil {
			break // the client is done (an overflow, a cut-off write, Close): nothing more is read from the stream
		}
	}
	lines.wipe()
	if failure != nil { // nil when the client had already failed
		k.fail(failure)
	}
	lines.drain() // keep the child from blocking on a full pipe
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
// before its reply fields are read: a notification (no id) is ignored whatever else it holds, unless OnEvent takes
// it, and a message with a method, even "", is a server request. Keys are exact.
//
// The line is read where it lies (wire.Member): a reply is checked in place, and its result and error message are
// copied out only for the call waiting for it (deliver). An event copies its method and params for the handler.
func (k *core) handle(line []byte) bool {
	if len(line) == 0 {
		return true
	}
	if !ValidJSON(line) {
		return false
	}
	root := wire.Root(line)
	switch wire.KindOf(line, root) {
	case 'n':
		return true
	case '{':
	default:
		return false
	}
	id, hasID := present(line, root, "id")
	if !hasID {
		k.queueEvent(line, root, false)
		return true // a notification, whatever else it holds
	}
	if method, ok := present(line, root, "method"); ok {
		if wire.KindOf(line, method) != '"' {
			return false
		}
		k.refuse(bytes.Clone(line[id.Start:id.End]))
		k.queueEvent(line, root, true)
		return true
	}
	var reply func() response // made only for a call that waits for it
	if e, ok := present(line, root, "error"); ok {
		if wire.KindOf(line, e) != '{' {
			return false
		}
		var code int64
		if c, ok := present(line, e, "code"); ok {
			if code, ok = wire.Int64Of(line, c); !ok {
				return false
			}
		}
		msg, hasMsg := present(line, e, "message")
		if hasMsg && wire.KindOf(line, msg) != '"' {
			return false
		}
		reply = func() response {
			rpc := &RPCError{Code: code}
			if hasMsg {
				rpc.Message = scrubbed(line, msg)
			}
			return response{err: rpc}
		}
	} else if r, ok := wire.Member(line, root, "result"); ok {
		reply = func() response { return response{result: bytes.Clone(line[r.Start:r.End]), hasResult: true} }
	} else {
		reply = func() response { return response{} }
	}
	if wire.KindOf(line, id) != '0' {
		return true // not one of ours
	}
	n, ok := wire.Int64Of(line, id)
	if !ok {
		return true // a fraction or an exponent: not one of ours
	}
	k.deliver(strconv.FormatInt(n, 10), line, reply)
	return true
}

// outbound is one message for writeLoop: a request or notification of a call, an answer to a server request
// (refusal), or, with no bytes, a marker Close waits for.
type outbound struct {
	b        []byte
	refusal  bool
	ctx      context.Context // the caller's; nil for a refusal or a marker
	deadline time.Time       // the caller's; a refusal's starts when its write does
	done     chan error      // the outcome, for a caller or Close; nil for a refusal
	state    int             // guarded by mu
}

const (
	outQueued = iota
	outStarted
	outAbandoned // its caller gave up before writeLoop reached it: skipped
	outReserved  // an answer admitted before its bytes are encoded: writeLoop waits at it
)

// enqueue appends o to the outbound queue; false once the client has failed.
func (k *core) enqueue(o *outbound) bool {
	k.mu.Lock()
	if k.failure != nil {
		k.mu.Unlock()
		return false
	}
	k.out = append(k.out, o)
	if o.refusal {
		k.refusalBytes += len(o.b)
	}
	k.mu.Unlock()
	select {
	case k.outWake <- struct{}{}:
	default: // a wake-up is already pending
	}
	return true
}

// refuse queues the answer to a server request (an approval, for instance): an error, written by writeLoop in its
// turn. The reader never writes, so a child that blocks on its full output while we wait to write its input cannot
// stall it. The answer is admitted to the queue as soon as the server request is read, before it is encoded, so a
// request queued while it is encoded is written after it; writeLoop waits at its slot until the bytes are in.
// Refusals beyond refusalCap, from a child that sends requests without reading the answers, fail the client. id is
// the request's id as the child wrote it.
func (k *core) refuse(id []byte) {
	slot := &outbound{refusal: true, state: outReserved}
	if !k.enqueue(slot) {
		return // failed: the queue is gone
	}
	if k.testRefusalAdmitted != nil {
		k.testRefusalAdmitted()
	}
	msg := map[string]any{"id": json.RawMessage(id), "error": map[string]any{"code": CodeMethodNotFound, "message": "server requests are not supported"}}
	b, err := json.Marshal(msg)
	if err != nil { // id is a parsed JSON value, so this cannot happen
		k.fail(codexError("Codex app-server sent a server request that could not be answered", nil))
		return
	}
	b = append(b, '\n')
	k.mu.Lock()
	if k.failure != nil { // a failure dropped the queue, the slot with it
		k.mu.Unlock()
		return
	}
	if k.refusalBytes+len(b) > refusalCap {
		k.mu.Unlock()
		k.fail(codexError("Codex app-server sent server requests without reading the answers", nil))
		return
	}
	slot.b, slot.state = b, outQueued
	k.refusalBytes += len(b)
	k.mu.Unlock()
	select {
	case k.outWake <- struct{}{}:
	default: // a wake-up is already pending
	}
}

// writeLoop writes the queued messages in order until the client fails. A request's write keeps its caller's
// deadline and cancellation; a caller that gave up before its turn is skipped. A refusal gets the per-call timeout
// from when its write starts, and one that cannot be written fails the client: the child would wait for an answer
// that never comes, and every later request would be written ahead of it.
func (k *core) writeLoop() {
	for {
		select {
		case <-k.outWake:
		case <-k.dead:
			return
		}
		for {
			k.mu.Lock()
			if k.failure != nil {
				k.mu.Unlock()
				return
			}
			if len(k.out) == 0 {
				k.mu.Unlock()
				break
			}
			o := k.out[0]
			if o.state == outReserved { // an answer still being encoded: refuse wakes us when it is in
				k.mu.Unlock()
				if k.testWriterWaiting != nil {
					k.testWriterWaiting()
				}
				break
			}
			k.out[0] = nil
			k.out = k.out[1:]
			if o.state == outAbandoned {
				k.mu.Unlock()
				continue
			}
			o.state = outStarted
			k.mu.Unlock()
			switch {
			case o.b == nil: // Close's marker: everything queued before it is written
				o.done <- nil
			case o.refusal:
				err := k.writeBytes(context.Background(), o.b, time.Now().Add(k.timeout))
				k.mu.Lock()
				if k.failure == nil { // a failure released the counts with the queue
					k.refusalBytes -= len(o.b)
				}
				k.mu.Unlock()
				if err != nil {
					k.fail(codexError("Codex app-server did not read the answer to a server request", nil))
					return
				}
			default:
				o.done <- k.writeBytes(o.ctx, o.b, o.deadline)
			}
		}
	}
}

// deliver hands the reply in line to the call waiting for it. The reply is made, by reply, only for a call that
// waits, under the lock the call takes to stop waiting: so a reply is either taken by its call, which clears its
// result, or never made. The line is cleared before the call is given the reply. A reply read after the client
// failed is dropped: the failure came first, so the call fails with it.
func (k *core) deliver(id string, line []byte, reply func() response) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.failure != nil {
		return
	}
	ch, ok := k.pending[id]
	if !ok {
		return
	}
	delete(k.pending, id)
	resp := reply()
	clear(line)
	ch <- resp // buffered, and this is its one reply: never blocks
}

// fail marks the client unusable and releases every pending call. The outbound queue is dropped with it: nothing is
// written after a failure, so a failed or closed client keeps none of the encoded messages, and each caller still
// queued sees dead and returns the failure. A write under way is interrupted by moving its deadline, so a child that
// resumes reading gets no more of it and its caller returns the failure, not a timeout.
func (k *core) fail(err error) {
	k.mu.Lock()
	if k.failure == nil {
		k.failure = err
		close(k.dead)
	}
	clear(k.out)
	k.out, k.refusalBytes = nil, 0
	if k.writing {
		_ = k.stdin.SetWriteDeadline(time.Now())
	}
	k.mu.Unlock()
}

func (k *core) failed() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.failure
}

// send queues one message for writeLoop and waits for its write, giving up at deadline and when ctx ends: a child
// that stops reading cannot hold a call past its timeout. Waiting for the messages queued before it counts against
// the deadline too. A caller that gives up before its turn takes its message off the queue; one whose write has
// started waits for it, which its deadline and ctx interrupt. A write cut off after part of the message was sent
// leaves the stream unusable, so the client is failed.
func (k *core) send(ctx context.Context, v any, deadline time.Time) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("codexapp: encode: %w", err)
	}
	timedOut := codexError("Codex app-server did not accept a request within "+k.timeout.String(), nil)
	if err := ctx.Err(); err != nil {
		return err
	}
	o := &outbound{b: append(b, '\n'), ctx: ctx, deadline: deadline, done: make(chan error, 1)}
	if !k.enqueue(o) {
		return k.failed()
	}
	wait := time.NewTimer(time.Until(deadline))
	defer wait.Stop()
	var gaveUp error
	select {
	case err := <-o.done:
		return err
	case <-k.dead:
		gaveUp = nil // the failure, read below
	case <-ctx.Done():
		gaveUp = ctx.Err()
	case <-wait.C:
		gaveUp = timedOut
	}
	k.mu.Lock()
	queued := o.state == outQueued
	if queued {
		o.state = outAbandoned
	}
	failure := k.failure
	k.mu.Unlock()
	if !queued {
		// Started: writeLoop reports every write it starts, which its deadline, ctx and Close all end.
		return <-o.done
	}
	if gaveUp == nil {
		return failure
	}
	return gaveUp
}

// writeBytes writes one encoded message, ended by a newline, for writeLoop: it gives up at deadline and when ctx
// ends, and a failure (Close's included) interrupts it and is what it returns.
func (k *core) writeBytes(ctx context.Context, b []byte, deadline time.Time) error {
	timedOut := codexError("Codex app-server did not accept a request within "+k.timeout.String(), nil)
	if err := ctx.Err(); err != nil {
		return err
	}
	if !time.Now().Before(deadline) {
		return timedOut
	}
	if err := k.stdin.SetWriteDeadline(deadline); err != nil {
		return codexError("Codex app-server is not accepting requests", nil)
	}
	// Marked under the lock fail takes, after the deadline is set: a failure from here on moves the deadline to now,
	// and one before it is seen here, so no failure lets the write run on.
	k.mu.Lock()
	if err := k.failure; err != nil {
		k.mu.Unlock()
		return err
	}
	k.writing = true
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		k.writing = false
		k.mu.Unlock()
	}()
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
	failure := k.failed() // one that interrupted the write, read before a cut-off adds its own
	if n > 0 {
		k.fail(codexError("Codex app-server stream was cut off inside a request", nil))
	}
	switch {
	case failure != nil:
		return failure
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, os.ErrDeadlineExceeded):
		return timedOut
	}
	return codexError("Codex app-server is not accepting requests", nil)
}

// call is callRaw with the result decoded into out, when non-nil: out is the caller's, and no longer the package's
// to clear.
func (k *core) call(ctx context.Context, method string, params, out any) error {
	var decode func([]byte) error
	if out != nil {
		decode = func(result []byte) error {
			if err := json.Unmarshal(result, out); err != nil {
				return codexError("Codex app-server sent an unexpected "+method+" result", nil)
			}
			return nil
		}
	}
	return k.callRaw(ctx, method, params, decode)
}

// callRaw sends one request and hands its result to use, when non-nil, whose error it returns. The result is a copy
// of the call's own, cleared once use returns; a reply that comes after the call stopped waiting is cleared here, or
// never copied (deliver).
func (k *core) callRaw(ctx context.Context, method string, params any, use func(result []byte) error) error {
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
		select {
		case resp := <-ch: // delivered once the call had stopped waiting: deliver sends under this lock
			clear(resp.result)
		default:
		}
		k.mu.Unlock()
	}()

	if params == nil {
		params = map[string]any{}
	}
	deadline, _ := ctx.Deadline() // set: ctx carries the per-call timeout
	if err := k.send(ctx, map[string]any{"id": id, "method": method, "params": params}, deadline); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return codexError("Codex app-server did not accept "+method+" within "+k.timeout.String(), nil)
		}
		return err
	}
	answer := func(resp response) error {
		defer clear(resp.result)
		if resp.err != nil {
			return codexError("Codex app-server rejected "+method, resp.err)
		}
		if !resp.hasResult { // neither result nor error, whether or not the caller reads the result
			return codexError("Codex app-server sent an unexpected "+method+" result", nil)
		}
		if use != nil {
			return use(resp.result)
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
		k.closeEvents() // before fail wakes eventLoop, so no further event is admitted
		// The messages queued before Close, answers to server requests included, are written first, for at most
		// closeFlush: a request the child sent before Close is answered rather than dropped with the queue.
		marker := &outbound{done: make(chan error, 1)}
		if k.enqueue(marker) {
			flush := time.NewTimer(closeFlush)
			select {
			case <-marker.done:
			case <-k.dead:
			case <-flush.C:
			}
			flush.Stop()
		}
		k.fail(codexError("Codex app-server was closed", nil))
		_ = k.stdin.Close()
		pid := k.cmd.Process.Pid
		// Once the child has exited and its group is empty nothing is signalled. The child is not reaped until the end
		// of Close, so its group ID cannot name another group meanwhile.
		if !k.gone(pid) {
			k.signalGroup(pid, syscall.SIGTERM)
		}
		if !k.waitGroup(pid, time.Now().Add(killGrace)) {
			k.signalGroup(pid, syscall.SIGKILL) // the group was alive a moment ago, so its ID is still ours
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
		// The group is no longer signalled, so the child is reaped now, or once it exits if it was abandoned.
		select {
		case <-k.exited:
			_ = k.cmd.Wait()
		default:
			go func() {
				<-k.exited
				_ = k.cmd.Wait()
			}()
		}
		if n := k.stderr.Len(); n > 0 {
			k.logger.Debug("codex app-server wrote to stderr", "bytes", n)
		}
	})
	return nil
}

// signalGroup sends sig to the child's process group while the child is our unreaped child, whose ID, and so the
// group ID, cannot name another group. A child the kernel reaped itself is not signalled: its ID may already be
// another process's.
func (k *core) signalGroup(pid int, sig syscall.Signal) {
	if !held(pid) {
		k.logger.Debug("codex app-server was reaped by the system; its group is not signalled", "pid", pid)
		return
	}
	if k.testSignal != nil {
		k.testSignal(pid, sig)
		return
	}
	_ = syscall.Kill(-pid, sig)
}

// gone reports whether the child has exited and no live member of its process group remains. Once the kernel has
// reaped the child, a group with its ID is not ours, so the answer is yes.
func (k *core) gone(pid int) bool {
	select {
	case <-k.exited:
		if k.lost {
			return true
		}
		if k.testLiveGroup != nil {
			return !k.testLiveGroup(pid)
		}
		return !groupAlive(pid)
	default:
		return false
	}
}

// waitGroup reports whether the child has exited and no live member of its process group remains before deadline.
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
