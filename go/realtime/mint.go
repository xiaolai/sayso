// Package realtime mints ephemeral client secrets, dials realtime WebSocket sessions and decodes their events
// (spec/http-and-mint.md, spec/session.md). Session shape, pooling, retry and turn policy belong to the caller.
package realtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(mintBody{Session: session, ExpiresAfter: expiresAfter{Anchor: "created_at", Seconds: int64(lifetime / time.Second)}}); err != nil {
		return nil, fmt.Errorf("realtime: encode mint body: %w", err)
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
		return secret.ClientSecret{}, &openaihttp.Error{Class: openaihttp.ClassAuthExpired, Message: "the refreshed sign-in was refused too", Err: err}
	}
	return sec, err
}

// post makes one mint request. Neither the request nor the response body is ever logged: both carry or yield
// secrets.
func (m *Minter) post(ctx context.Context, tok secret.Token, body []byte) (secret.ClientSecret, error) {
	ctx, cancel := context.WithTimeout(ctx, m.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(m.BaseURL, "/")+mintPath, bytes.NewReader(body))
	if err != nil {
		return secret.ClientSecret{}, &openaihttp.Error{Class: openaihttp.ClassInternal, Message: "realtime: mint request", Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+tok.Bearer())
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.Client.Do(req)
	if err != nil {
		if !isClassified(err) {
			return secret.ClientSecret{}, openaihttp.TransportError("realtime: mint", err)
		}
		return secret.ClientSecret{}, fmt.Errorf("realtime: mint: %w", err)
	}
	if err := openaihttp.CheckResponse(resp, m.Route); err != nil {
		return secret.ClientSecret{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMintReply+1))
	defer clear(raw) // the reply holds the secret
	if err != nil {
		return secret.ClientSecret{}, openaihttp.TransportError("realtime: mint: read reply", err)
	}
	if len(raw) > maxMintReply {
		return secret.ClientSecret{}, &openaihttp.Error{Class: openaihttp.ClassProtocol, Message: "mint reply exceeds 1 MiB"}
	}
	var reply struct {
		Value     *string          `json:"value"`
		ExpiresAt *json.RawMessage `json:"expires_at"`
	}
	if json.Unmarshal(raw, &reply) != nil {
		return secret.ClientSecret{}, &openaihttp.Error{Class: openaihttp.ClassProtocol, Message: "mint reply is not valid JSON"}
	}
	var expires int64
	if reply.ExpiresAt == nil || json.Unmarshal(*reply.ExpiresAt, &expires) != nil {
		return secret.ClientSecret{}, &openaihttp.Error{Class: openaihttp.ClassProtocol, Message: "mint reply lacks an integer expires_at"}
	}
	if reply.Value == nil || *reply.Value == "" || len(*reply.Value) > maxSecretBytes {
		return secret.ClientSecret{}, &openaihttp.Error{Class: openaihttp.ClassProtocol, Message: "mint reply value is missing, empty or longer than 4096 bytes"}
	}
	return secret.NewClientSecret(*reply.Value, time.Unix(expires, 0)), nil
}

// isClassified reports an *openaihttp.Error in the chain: the origin policy's refusal (internal by design) or a
// refused redirect (protocol). Any other failure of Do is a broken connection.
func isClassified(err error) bool {
	var e *openaihttp.Error
	return errors.As(err, &e)
}
