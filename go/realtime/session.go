package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/secret"
)

// closeGrace is how long Close waits for the library's close handshake before dropping the connection; the library
// alone may block for 10 s.
const closeGrace = 2 * time.Second

// ErrClosed is returned by Next and Send after Close.
var ErrClosed = errors.New("realtime: session closed")

// WebSocketURL is the realtime endpoint for model under base (openaihttp.APIBase in production). The https scheme is
// kept: the library upgrades it, and the origin policy only knows https.
func WebSocketURL(base, model string) string {
	return strings.TrimRight(base, "/") + "/v1/realtime?model=" + url.QueryEscape(model)
}

// DialConfig describes one WebSocket session.
type DialConfig struct {
	Client *http.Client // from openaihttp.NewClient: proxy and origin policy apply
	URL    string
	Secret secret.ClientSecret
	// HandshakeTimeout bounds the opening handshake; required.
	HandshakeTimeout time.Duration
	// ReadLimit is the largest message accepted, in bytes; required. A longer message is a protocol error and is
	// never decoded.
	ReadLimit int64
	Route     string       // named on a route-denied handshake refusal; optional
	Logger    *slog.Logger // nil discards
	// Trace, if set, receives every message as sent or received ("client" or "server"), before decoding. It must not
	// keep raw. It runs on the session's reader and writers, so it must return promptly and must not call Close,
	// Next or Send on the session: Close waits for the reader, which waits for Trace. To end the session from Trace,
	// call Close from another goroutine.
	Trace func(dir string, raw []byte)
}

// Session is one realtime WebSocket (NET-03): one reader goroutine feeds a channel, writes are serialised, and no
// client pings are ever sent: liveness comes from the caller's own timers.
type Session struct {
	conn   *websocket.Conn
	raw    io.Closer // the upgraded connection under conn, nil if not captured
	logger *slog.Logger
	trace  func(dir string, raw []byte)

	writeSem chan struct{} // one token: the write in progress

	events     chan item
	closing    chan struct{}
	readerDone chan struct{}
	readErr    error // set by the reader before it closes events

	unknownMu sync.Mutex
	unknown   map[string]int

	closeOnce sync.Once
}

type item struct {
	ev  Event
	err error
}

// Open obtains a secret for session from src and dials with it; cfg.Secret is ignored. The configuration and the
// destination are checked first, so a programming error never costs a sign-in or a mint.
func Open(ctx context.Context, src SecretSource, session any, cfg DialConfig) (*Session, error) {
	if err := cfg.check(); err != nil {
		return nil, err
	}
	if err := openaihttp.CheckOrigin(cfg.Client, cfg.URL); err != nil {
		return nil, err
	}
	sec, err := src.Secret(ctx, session)
	if err != nil {
		return nil, err
	}
	cfg.Secret = sec
	return Dial(ctx, cfg)
}

// check refuses a configuration no dial can use, a programming error.
func (cfg DialConfig) check() error {
	// math.MaxInt64 is refused: the library adds one to the limit, which would wrap and disable it.
	if !openaihttp.Guarded(cfg.Client) || cfg.HandshakeTimeout <= 0 || cfg.ReadLimit <= 0 || cfg.ReadLimit == math.MaxInt64 {
		return &openaihttp.Error{Class: openaihttp.ClassInternal, Message: "realtime: Dial needs a client from openaihttp.NewClient, a HandshakeTimeout and a ReadLimit below math.MaxInt64"}
	}
	// A scheme the library cannot dial is a configuration error, as in Swift, not a network failure.
	if u, err := url.Parse(cfg.URL); err != nil || !dialable[u.Scheme] || u.Host == "" {
		return &openaihttp.Error{Class: openaihttp.ClassInternal, Message: "realtime: Dial needs a ws, wss, http or https URL"}
	}
	return nil
}

// Dial opens the session with `Authorization: Bearer ek_…` within cfg.HandshakeTimeout.
func Dial(ctx context.Context, cfg DialConfig) (*Session, error) {
	if err := cfg.check(); err != nil {
		return nil, err
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	dctx, cancel := context.WithTimeout(ctx, cfg.HandshakeTimeout)
	defer cancel()
	capture := &upgradeCapture{rt: cfg.Client.Transport}
	hc := *cfg.Client
	hc.Transport = capture
	// The library reads a malformed 101's upgraded body during its cleanup for up to 3 s whatever dctx says, so the
	// upgraded connection is closed when dctx ends before Dial returns.
	stop := context.AfterFunc(dctx, capture.close)
	conn, resp, err := websocket.Dial(dctx, cfg.URL, &websocket.DialOptions{
		HTTPClient: &hc,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + cfg.Secret.Bearer()}},
	})
	if !stop() && err == nil {
		_ = conn.CloseNow() // the deadline closed the connection as Dial succeeded
		return nil, openaihttp.TransportError("realtime: open WebSocket", dctx.Err())
	}
	if err != nil {
		var e *openaihttp.Error
		if errors.As(err, &e) {
			// The origin policy's refusal, without the wrappers around it, whose text could quote a URL.
			refused := *e
			return nil, &refused
		}
		if ctx.Err() != nil {
			return nil, openaihttp.TransportError("realtime: open WebSocket", ctx.Err()) // the caller's, before any reply
		}
		if resp != nil {
			return nil, handshakeRefused(resp, cfg.Route)
		}
		return nil, openaihttp.TransportError("realtime: open WebSocket", err)
	}
	conn.SetReadLimit(cfg.ReadLimit)
	s := &Session{
		conn:       conn,
		raw:        capture.captured(),
		logger:     logger,
		trace:      cfg.Trace,
		writeSem:   make(chan struct{}, 1),
		events:     make(chan item),
		closing:    make(chan struct{}),
		readerDone: make(chan struct{}),
		unknown:    map[string]int{},
	}
	go s.readLoop()
	return s, nil
}

// readLoop is the only reader. It ends when the connection fails or Close runs.
func (s *Session) readLoop() {
	defer close(s.readerDone)
	defer close(s.events)
	for {
		_, data, err := s.conn.Read(context.Background())
		if err != nil {
			select {
			case <-s.closing:
				s.readErr = ErrClosed
			default:
				s.readErr = ReadError(err)
			}
			return
		}
		if s.trace != nil {
			s.trace("server", data)
		}
		ev, err := DecodeEvent(data)
		if u, ok := ev.(Unknown); ok && err == nil {
			s.countUnknown(u.Type) // delivered too: an app may read a type the package does not decode
		}
		select {
		case s.events <- item{ev: ev, err: err}:
		case <-s.closing:
			s.readErr = ErrClosed
			return
		}
	}
}

func (s *Session) countUnknown(typ string) {
	s.unknownMu.Lock()
	s.unknown[typ]++
	n := s.unknown[typ]
	s.unknownMu.Unlock()
	if n == 1 {
		s.logger.Debug("realtime: unknown event type", "type", secret.Scrub(typ))
	}
}

// Unknown returns how many events of each unknown type this session received: API drift (NET-04).
func (s *Session) Unknown() map[string]int {
	s.unknownMu.Lock()
	defer s.unknownMu.Unlock()
	return maps.Clone(s.unknown)
}

// Next returns the next event, ignored and unknown types included. A protocol error for one malformed event does not
// end the session; a connection failure does, and is returned classified from then on, until Close, after which Next
// returns ErrClosed. Cancelling ctx unblocks it with ctx.Err().
func (s *Session) Next(ctx context.Context) (Event, error) {
	// A cancelled ctx returns before select, which could otherwise take a pending event and lose it.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-s.closing:
		return nil, ErrClosed // after Close, an earlier connection failure included, as in Swift
	default:
	}
	select {
	case it, ok := <-s.events:
		if !ok {
			select {
			case <-s.closing:
				return nil, ErrClosed
			default:
			}
			return nil, s.readErr // written before events was closed
		}
		return it.ev, it.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Send writes one client event as JSON, which must be valid UTF-8: a json.RawMessage is passed through as it is, and
// invalid UTF-8 is an internal error that sends nothing. Writes are serialised; waiting for an earlier write observes
// ctx and Close.
func (s *Session) Send(ctx context.Context, event any) error {
	if err := ctx.Err(); err != nil {
		return err // cancelled before the call: nothing is encoded or sent, and the session is untouched
	}
	b, err := json.Marshal(event)
	if err != nil {
		// A fixed message: a custom marshaller's error text could carry anything.
		return &openaihttp.Error{Class: openaihttp.ClassInternal, Message: "realtime: the event does not encode"}
	}
	if !utf8.Valid(b) {
		// A text message is UTF-8, as in Swift; the peer would refuse the frame, or read a different event.
		return &openaihttp.Error{Class: openaihttp.ClassInternal, Message: "realtime: the event is not valid UTF-8"}
	}
	select {
	case <-s.closing:
		return ErrClosed
	default:
	}
	select {
	case s.writeSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.closing:
		return ErrClosed
	}
	defer func() { <-s.writeSem }()
	select {
	case <-s.closing:
		return ErrClosed // closed while this write waited
	default:
	}
	if err := ctx.Err(); err != nil {
		// A select picks at random among ready cases, so a cancelled call can win the semaphore; it must not reach
		// the library, whose cancellation handling closes the connection.
		return err
	}
	if s.trace != nil {
		s.trace("client", b)
	}
	if err := s.conn.Write(ctx, websocket.MessageText, b); err != nil {
		select {
		case <-s.closing:
			return ErrClosed // our own Close ended the write
		default:
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return openaihttp.TransportError("realtime: send", err)
	}
	return nil
}

// Close runs the library's close handshake in a goroutine and drops the connection if it has not finished within
// 2 s. It waits for the reader goroutine, so nothing outlives it. Safe to call more than once.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		close(s.closing)
		done := make(chan error, 1)
		go func() { done <- s.conn.Close(websocket.StatusNormalClosure, "") }()
		t := time.NewTimer(closeGrace)
		defer t.Stop()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				s.logger.Debug("realtime: close handshake", "err", secret.Scrub(err.Error()))
			}
		case <-t.C:
			s.logger.Debug("realtime: peer did not answer the close frame; dropping the connection")
			// CloseNow is a no-op while Close is in progress (the library marks the connection closing first), so the
			// upgraded connection is closed under it; that fails the library's pending read and Close returns.
			if s.raw != nil {
				_ = s.raw.Close()
			}
			_ = s.conn.CloseNow()
			<-done
		}
		<-s.readerDone
	})
	return nil
}

// handshakeRefused classifies a handshake the server answered without a valid upgrade (spec/session.md): from its
// status and request id like an HTTP reply, never from its body, which URLSession does not expose to the Swift
// implementation; a reply that should have upgraded (a 2xx, or a 101 the library refused) is protocol.
func handshakeRefused(resp *http.Response, route string) error {
	if resp.StatusCode != http.StatusSwitchingProtocols {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		resp.Body = http.NoBody
		if err := openaihttp.CheckResponse(resp, route); err != nil {
			return err
		}
	}
	return &openaihttp.Error{Class: openaihttp.ClassProtocol, Status: resp.StatusCode,
		RequestID: secret.Scrub(resp.Header.Get("x-request-id")), Message: "realtime handshake: not a WebSocket upgrade"}
}

var dialable = map[string]bool{"ws": true, "wss": true, "http": true, "https": true}

// upgradeCapture keeps the upgraded connection of the handshake response, so Close can drop it when the library's
// close handshake hangs, and Dial when its deadline passes. It wraps the client's transport, so the origin policy and
// proxy still apply.
type upgradeCapture struct {
	rt   http.RoundTripper
	mu   sync.Mutex
	conn io.Closer
	done bool // closed by the deadline: a connection captured later is closed at once
}

func (u *upgradeCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := u.rt.RoundTrip(req)
	if err == nil && resp.StatusCode != http.StatusSwitchingProtocols {
		// A refusal is classified from its status and request id alone (spec/session.md); dropping the body here
		// keeps the library's cleanup from reading it, which could stall the return. An injected transport may
		// return no body at all.
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		resp.Body = http.NoBody
	}
	if err == nil && resp.StatusCode == http.StatusSwitchingProtocols {
		u.mu.Lock()
		u.conn = resp.Body // an io.ReadWriteCloser for a 101
		late := u.done
		u.mu.Unlock()
		if late {
			_ = resp.Body.Close()
		}
	}
	return resp, err
}

func (u *upgradeCapture) close() {
	u.mu.Lock()
	u.done = true
	c := u.conn
	u.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

func (u *upgradeCapture) captured() io.Closer {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.conn
}

// ReadError classifies a failed read: a message over the read limit is a protocol error, a close frame is classified
// by its code, anything else is a transport failure.
func ReadError(err error) error {
	if errors.Is(err, websocket.ErrMessageTooBig) {
		return &openaihttp.Error{Class: openaihttp.ClassProtocol, Code: "1009", Message: "realtime message exceeds the read limit", Err: err}
	}
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return openaihttp.ConnClosed(int(ce.Code), ce.Reason, err)
	}
	return openaihttp.TransportError("realtime: read", err)
}
