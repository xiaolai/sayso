// Package realtime mints ephemeral client secrets, dials realtime WebSocket sessions and decodes their events
// (spec/http-and-mint.md, spec/session.md). Session shape, pooling, retry and turn policy belong to the caller.
package realtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xiaolai/sayso/go/internal/bounded"
	"github.com/xiaolai/sayso/go/internal/wire"
	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/secret"
)

const (
	mintPath = "/v1/realtime/client_secrets"
	// maxMintReply bounds the mint reply; a longer reply is a protocol error.
	maxMintReply = 1 << 20
	// maxSecretBytes bounds the minted value.
	maxSecretBytes = 4096
)

// Credentials yields the sign-in bearer and refreshes it after a refusal; *codexauth.Credentials implements it.
type Credentials interface {
	Token(ctx context.Context) (secret.Token, error)
	RefreshAfter(ctx context.Context, stale secret.Token) (secret.Token, error)
}

// SecretSource yields a client secret for a session object. Sessions take a SecretSource, never a sign-in, so a
// secret obtained elsewhere (a remote service, another process) changes no API. *Minter implements it.
type SecretSource interface {
	Secret(ctx context.Context, session any) (secret.ClientSecret, error)
}

// Minter mints ephemeral client secrets (NET-02).
type Minter struct {
	Client   *http.Client // from openaihttp.NewClient
	BaseURL  string       // openaihttp.APIBase; tests use a reserved test origin
	Creds    Credentials
	Timeout  time.Duration // per request; required
	Lifetime time.Duration // the secret's asked lifetime, whole seconds; required
	Route    string        // named on a route-denied error; optional
}

// The wire shape of the mint body; the session is the caller's, passed through unchanged.
type mintBody struct {
	Session      any          `json:"session"`
	ExpiresAfter expiresAfter `json:"expires_after"`
}

type expiresAfter struct {
	Anchor  string `json:"anchor"`
	Seconds int64  `json:"seconds"`
}

// MintBody returns the JSON mint body for session and lifetime. HTML escaping is off, so the body shows text such as
// instructions as written.
func MintBody(session any, lifetime time.Duration) ([]byte, error) {
	if session == nil {
		return nil, errors.New("realtime: nil session")
	}
	if lifetime <= 0 || lifetime%time.Second != 0 {
		return nil, fmt.Errorf("realtime: lifetime %v is not a positive whole number of seconds", lifetime)
	}
	// The session must encode as one strict JSON object, as Swift requires of its bytes. The encoder's error is not
	// passed on: it can quote a session value (a json.Number, a marshaler's message).
	var sb bytes.Buffer
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(session); err != nil {
		return nil, errors.New("realtime: the session does not encode as JSON")
	}
	encoded := bytes.TrimSuffix(sb.Bytes(), []byte("\n"))
	if root, err := wire.Parse(encoded); err != nil || root.Kind() != '{' {
		return nil, errors.New("realtime: the session is not a JSON object")
	}
	var b bytes.Buffer
	enc = json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(mintBody{Session: json.RawMessage(encoded), ExpiresAfter: expiresAfter{Anchor: "created_at", Seconds: int64(lifetime / time.Second)}}); err != nil {
		return nil, errors.New("realtime: the mint body does not encode as JSON")
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// Secret mints a client secret for session. On auth-expired it asks Creds to refresh after that token and retries
// once; a second auth-expired is returned as auth-expired.
func (m *Minter) Secret(ctx context.Context, session any) (secret.ClientSecret, error) {
	if !openaihttp.Guarded(m.Client) || m.Creds == nil || m.Timeout <= 0 {
		return secret.ClientSecret{}, &openaihttp.Error{Class: openaihttp.ClassInternal, Message: "realtime: Minter needs a client from openaihttp.NewClient, Creds and a Timeout"}
	}
	body, err := MintBody(session, m.Lifetime)
	if err != nil {
		return secret.ClientSecret{}, &openaihttp.Error{Class: openaihttp.ClassInternal, Message: err.Error()}
	}
	// The destination first, as Open checks it: a wrong BaseURL is a programming error, which must not cost a
	// sign-in (a Codex start, perhaps a refresh) before the request is refused.
	if err := openaihttp.CheckOrigin(m.Client, strings.TrimRight(m.BaseURL, "/")+mintPath); err != nil {
		var e *openaihttp.Error
		if errors.As(err, &e) {
			refused := *e
			refused.Message = "realtime: mint: " + e.Message
			return secret.ClientSecret{}, &refused
		}
		return secret.ClientSecret{}, err
	}
	tok, err := m.Creds.Token(ctx)
	if err != nil {
		return secret.ClientSecret{}, err
	}
	sec, err := m.post(ctx, tok, body)
	if openaihttp.Classify(err) != openaihttp.ClassAuthExpired {
		return sec, err
	}
	fresh, rerr := m.Creds.RefreshAfter(ctx, tok)
	if rerr != nil {
		return secret.ClientSecret{}, rerr
	}
	sec, err = m.post(ctx, fresh, body)
	if openaihttp.Classify(err) == openaihttp.ClassAuthExpired {
		refused := &openaihttp.Error{Class: openaihttp.ClassAuthExpired, Message: "the refreshed sign-in was refused too", Err: err}
		var e *openaihttp.Error
		if errors.As(err, &e) { // keep the second refusal's provider fields, as Swift does
			refused.Status, refused.Type, refused.Code, refused.Param, refused.RequestID = e.Status, e.Type, e.Code, e.Param, e.RequestID
		}
		return secret.ClientSecret{}, refused
	}
	return sec, err
}

// send posts the mint body and returns the reply. A request whose connection was lost after it was made and before
// any byte of a reply arrived is sent once more, within the same ctx (spec/http-and-mint.md, lost connection): the
// server may have closed a kept-alive connection as the request reached it, and net/http does not resend a POST. A
// secret minted for the lost request expires unused. A second loss, a refusal and an expired ctx are returned as they
// are.
func (m *Minter) send(ctx context.Context, tok secret.Token, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.BaseURL, "/")+mintPath, bytes.NewReader(body))
		if err != nil {
			return nil, &openaihttp.Error{Class: openaihttp.ClassInternal, Message: "realtime: mint request", Err: err}
		}
		req.Header.Set("Authorization", "Bearer "+tok.Bearer())
		req.Header.Set("Content-Type", "application/json")
		var connected, answered atomic.Bool
		req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn:              func(httptrace.GotConnInfo) { connected.Store(true) },
			GotFirstResponseByte: func() { answered.Store(true) },
		}))
		resp, err := m.Client.Do(req)
		var refused *openaihttp.Error
		lost := err != nil && connected.Load() && !answered.Load() && ctx.Err() == nil && !errors.As(err, &refused)
		if !lost || attempt > 0 {
			return resp, err
		}
	}
}

// post makes one mint request. Neither the request nor the response body is ever logged: both carry or yield
// secrets.
func (m *Minter) post(ctx context.Context, tok secret.Token, body []byte) (secret.ClientSecret, error) {
	ctx, cancel := context.WithTimeout(ctx, m.Timeout)
	defer cancel()
	resp, err := m.send(ctx, tok, body)
	if err != nil {
		var e *openaihttp.Error
		if !errors.As(err, &e) {
			return secret.ClientSecret{}, openaihttp.TransportError("realtime: mint", err)
		}
		// The policy's or the redirect refusal, without the *url.Error around it, whose text quotes the redirect's
		// Location verbatim.
		refused := *e
		refused.Message = "realtime: mint: " + e.Message
		return secret.ClientSecret{}, &refused
	}
	if err := openaihttp.CheckResponse(resp, m.Route); err != nil {
		return secret.ClientSecret{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	// The reply holds the secret: it is read into one buffer, cleared on every exit, and read where it lies.
	raw, err := bounded.Read(resp.Body, resp.ContentLength, maxMintReply+1)
	defer clear(raw)
	if err != nil {
		return secret.ClientSecret{}, openaihttp.TransportError("realtime: mint: read reply", err)
	}
	// A reply refused after a 2xx keeps the status and request id, as an HTTP refusal does.
	refuse := func(msg string) error {
		return &openaihttp.Error{Class: openaihttp.ClassProtocol, Status: resp.StatusCode,
			RequestID: secret.Scrub(resp.Header.Get("x-request-id")), Message: msg}
	}
	if len(raw) > maxMintReply {
		return secret.ClientSecret{}, refuse("mint reply exceeds 1 MiB")
	}
	if !wire.Valid(raw) || wire.KindOf(raw, wire.Root(raw)) != '{' {
		return secret.ClientSecret{}, refuse("mint reply is not valid JSON")
	}
	root := wire.Root(raw)
	e, ok := wire.Member(raw, root, "expires_at")
	var expires int64
	if ok && wire.KindOf(raw, e) != 'n' {
		expires, ok = wire.Int64Of(raw, e)
	} else {
		ok = false
	}
	if !ok {
		return secret.ClientSecret{}, refuse("mint reply lacks an integer expires_at")
	}
	// The value is decoded into a buffer it cannot outgrow, checked there, and made a string only once it is kept.
	v, ok := wire.Member(raw, root, "value")
	if !ok || wire.KindOf(raw, v) != '"' {
		return secret.ClientSecret{}, refuse("mint reply value is missing, empty or longer than 4096 bytes")
	}
	value := make([]byte, 0, v.End-v.Start)
	defer clear(value[:cap(value)])
	value, _ = wire.AppendString(value, raw, v)
	if len(value) == 0 || len(value) > maxSecretBytes {
		return secret.ClientSecret{}, refuse("mint reply value is missing, empty or longer than 4096 bytes")
	}
	// The exact integer is kept; ExpiresAt clamps it to what a time.Time holds without wrapping.
	return secret.NewClientSecretUnix(string(value), expires), nil
}
