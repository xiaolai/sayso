package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

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
	return strings.TrimSuffix(base, "/") + "/v1/realtime?model=" + url.QueryEscape(model)
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
	// keep raw.
	Trace func(dir string, raw []byte)
}

// Session is one realtime WebSocket (NET-03): one reader goroutine feeds a channel, writes are serialised, and no
// client pings are ever sent: liveness comes from the caller's own timers.
type Session struct {
	conn   *websocket.Conn
	raw    io.Closer // the upgraded connection under conn, nil if not captured
	logger *slog.Logger
	trace  func(dir string, raw []byte)

	writeMu sync.Mutex

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

// Open obtains a secret for session from src and dials with it; cfg.Secret is ignored.
func Open(ctx context.Context, src SecretSource, session any, cfg DialConfig) (*Session, error) {
	sec, err := src.Secret(ctx, session)
	if err != nil {
		return nil, err
	}
	cfg.Secret = sec
	return Dial(ctx, cfg)
}

// Dial opens the session with `Authorization: Bearer ek_…` within cfg.HandshakeTimeout.
func Dial(ctx context.Context, cfg DialConfig) (*Session, error) {
	if !openaihttp.Guarded(cfg.Client) || cfg.HandshakeTimeout <= 0 || cfg.ReadLimit <= 0 {
		return nil, &openaihttp.Error{Class: openaihttp.ClassInternal, Message: "realtime: Dial needs a client from openaihttp.NewClient, a HandshakeTimeout and a ReadLimit"}
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
	conn, resp, err := websocket.Dial(dctx, cfg.URL, &websocket.DialOptions{
		HTTPClient: &hc,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + cfg.Secret.Bearer()}},
	})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			if cerr := openaihttp.CheckResponse(resp, cfg.Route); cerr != nil {
				return nil, cerr
			}
		}
		var e *openaihttp.Error
		if errors.As(err, &e) {
			return nil, err // the origin policy or a refused redirect
		}
		return nil, openaihttp.TransportError("realtime: open WebSocket", err)
	}
	conn.SetReadLimit(cfg.ReadLimit)
	s := &Session{
		conn:       conn,
		raw:        capture.conn,
		logger:     logger,
		trace:      cfg.Trace,
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
		if err == nil {
			if u, ok := ev.(Unknown); ok {
				s.countUnknown(u.Type)
				continue
			}
			if _, ok := ev.(Ignored); ok {
				continue
			}
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

// Next returns the next consumed event. A protocol error for one malformed event does not end the session; a
// connection failure does, and is returned classified from then on. Cancelling ctx unblocks it with ctx.Err().
func (s *Session) Next(ctx context.Context) (Event, error) {
	select {
	case it, ok := <-s.events:
		if !ok {
			return nil, s.readErr // written before events was closed
		}
		return it.ev, it.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Send writes one client event as JSON. Writes are serialised.
func (s *Session) Send(ctx context.Context, event any) error {
	b, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("realtime: encode event: %w", err)
	}
	select {
	case <-s.closing:
		return ErrClosed
	default:
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.trace != nil {
		s.trace("client", b)
	}
	if err := s.conn.Write(ctx, websocket.MessageText, b); err != nil {
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

// upgradeCapture keeps the upgraded connection of the handshake response, so Close can drop it when the library's
// close handshake hangs. It wraps the client's transport, so the origin policy and proxy still apply.
type upgradeCapture struct {
	rt   http.RoundTripper
	conn io.Closer
}

func (u *upgradeCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := u.rt.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusSwitchingProtocols {
		u.conn = resp.Body // an io.ReadWriteCloser for a 101
	}
	return resp, err
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
