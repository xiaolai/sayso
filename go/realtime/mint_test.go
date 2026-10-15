package realtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/internal/fakeopenai"
	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/realtime"
	"github.com/xiaolai/sayso/go/secret"
)

type fixedCreds struct{ bearer string }

func (f fixedCreds) Token(context.Context) (secret.Token, error) {
	return secret.NewToken(f.bearer, time.Time{}, "", "test"), nil
}

func (f fixedCreds) RefreshAfter(context.Context, secret.Token) (secret.Token, error) {
	return secret.NewToken(f.bearer, time.Time{}, "", "test"), nil
}

// The session must encode as one JSON object, as Swift requires; anything else is refused before a request.
func TestMintBodyNeedsAnObject(t *testing.T) {
	var typedNil *struct{ A int }
	for name, session := range map[string]any{
		"array":     []int{1},
		"scalar":    7,
		"typed nil": typedNil,
		"duplicate": json.RawMessage(`{"a":1,"a":2}`),
	} {
		if _, err := realtime.MintBody(session, time.Minute); err == nil {
			t.Errorf("%s: MintBody accepted it", name)
		}
	}
	body, err := realtime.MintBody(json.RawMessage(`{"model":"m"}`), time.Minute)
	if err != nil || string(body) != `{"session":{"model":"m"},"expires_after":{"anchor":"created_at","seconds":60}}` {
		t.Fatalf("MintBody = %s, %v", body, err)
	}
}

// An encoding error never quotes the session.
func TestMintBodyErrorQuotesNothing(t *testing.T) {
	_, err := realtime.MintBody(map[string]any{"x": json.Number("ek_canary_number")}, time.Minute)
	if err == nil || strings.Contains(err.Error(), "canary") {
		t.Fatalf("err = %v", err)
	}
}

// A refused redirect is classified without the redirect's Location in the error text, and every trailing slash of
// the base URL is removed.
func TestMintRedirectHidesLocation(t *testing.T) {
	var paths []string
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Location", "https://elsewhere.test/opaque-location-canary")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(hs.Close)
	c, base := fakeopenai.Client(t, hs.URL)
	m := &realtime.Minter{Client: c, BaseURL: base + "//", Creds: fixedCreds{"bearer-for-redirect"}, Timeout: 5 * time.Second, Lifetime: time.Minute}
	_, err := m.Secret(context.Background(), json.RawMessage(`{}`))
	if openaihttp.Classify(err) != openaihttp.ClassProtocol {
		t.Fatalf("class %q, err %v", openaihttp.Classify(err), err)
	}
	for _, text := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		if strings.Contains(text, "opaque-location-canary") {
			t.Fatal("the error quotes the redirect's Location")
		}
	}
	if len(paths) != 1 || paths[0] != "/v1/realtime/client_secrets" {
		t.Fatalf("paths %q", paths)
	}
}
