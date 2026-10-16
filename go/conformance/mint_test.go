package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/internal/fakeopenai"
	"github.com/xiaolai/sayso/go/internal/fixtures"
	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/realtime"
	"github.com/xiaolai/sayso/go/secret"
)

type mintReply struct {
	Status    int               `json:"status"`
	Headers   map[string]string `json:"headers"`
	JSON      json.RawMessage   `json:"json"`
	Body      *string           `json:"body"`
	BodyPadTo int               `json:"bodyPadTo"`
}

type mintCase struct {
	Setup struct {
		Origin          string          `json:"origin"`
		Session         json.RawMessage `json:"session"`
		LifetimeSeconds int             `json:"lifetimeSeconds"`
		Token           any             `json:"token"`
		RefreshTo       any             `json:"refreshTo"`
		Replies         []mintReply     `json:"replies"`
	} `json:"setup"`
	Expect map[string]any `json:"expect"`
}

// stubCreds returns token, and refreshTo after a refresh, counting both.
type stubCreds struct {
	token, refreshTo string
	mu               sync.Mutex
	tokens           int
	refreshes        int
}

func (s *stubCreds) Token(context.Context) (secret.Token, error) {
	s.mu.Lock()
	s.tokens++
	s.mu.Unlock()
	return secret.NewToken(s.token, time.Time{}, "", "conformance"), nil
}

func (s *stubCreds) RefreshAfter(context.Context, secret.Token) (secret.Token, error) {
	s.mu.Lock()
	s.refreshes++
	s.mu.Unlock()
	return secret.NewToken(s.refreshTo, time.Time{}, "", "conformance"), nil
}

type seenRequest struct {
	method, path, authorization, contentType string
	body                                     []byte
}

func TestMint(t *testing.T) {
	run(t, "mint", func(t *testing.T, c fixtures.Case) (fixtures.Outcome, string) {
		var mc mintCase
		if err := decodeExact(c, &mc); err != nil {
			return fixtures.Fail, err.Error()
		}
		return outcome(checkMint(t, mc))
	})
}

func checkMint(t *testing.T, mc mintCase) []string {
	token := fmt.Sprint(fixtures.Substitute(mc.Setup.Token))
	refreshTo := fmt.Sprint(fixtures.Substitute(mc.Setup.RefreshTo))
	var mu sync.Mutex
	var seen []seenRequest
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(seen)
		seen = append(seen, seenRequest{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), body})
		mu.Unlock()
		if n >= len(mc.Setup.Replies) {
			http.Error(w, "fixture has no reply for this request", http.StatusTeapot)
			return
		}
		rep := mc.Setup.Replies[n]
		for k, v := range rep.Headers {
			w.Header().Set(k, v)
		}
		out := []byte{}
		if rep.Body != nil {
			out = []byte(*rep.Body)
		} else if rep.JSON != nil {
			out = rep.JSON
		}
		if pad := rep.BodyPadTo - len(out); pad > 0 {
			out = append(out, strings.Repeat(" ", pad)...)
		}
		w.WriteHeader(rep.Status)
		_, _ = w.Write(out)
	}))
	defer hs.Close()
	client, base := fakeopenai.Client(t, hs.URL)
	if mc.Setup.Origin != "test" {
		base = mc.Setup.Origin
	}
	creds := &stubCreds{token: token, refreshTo: refreshTo}
	var session any
	if err := json.Unmarshal(mc.Setup.Session, &session); err != nil {
		return []string{"session: " + err.Error()}
	}
	m := &realtime.Minter{Client: client, BaseURL: base, Creds: creds, Timeout: 10 * time.Second,
		Lifetime: time.Duration(mc.Setup.LifetimeSeconds) * time.Second}
	sec, err := m.Secret(context.Background(), session)

	want := mc.Expect
	var p []string
	mu.Lock()
	requests := append([]seenRequest(nil), seen...)
	mu.Unlock()
	if class, _ := want["class"].(string); class == "" {
		if err != nil {
			return []string{"mint failed: " + err.Error()}
		}
		if w, ok := want["secret"]; ok && sec.Bearer() != w {
			p = append(p, "secret differs")
		}
		if w, ok := want["expiresAt"]; ok && !sameExact(sec.ExpiresAtUnix, w) {
			p = append(p, fmt.Sprintf("expiresAt %d, want %v", sec.ExpiresAtUnix, w))
		}
	} else {
		p = append(p, checkError(err, map[string]any{"class": class})...)
		var e *openaihttp.Error
		if errors.As(err, &e) {
			for key, got := range map[string]any{"status": e.Status, "type": e.Type, "code": e.Code, "param": e.Param, "requestId": e.RequestID} {
				if w, ok := want[key]; ok && !sameExact(got, w) {
					p = append(p, fmt.Sprintf("%s %v, want %v", key, got, w))
				}
			}
		}
		if err != nil {
			p = append(p, leaks(strings_(want["notInDescription"]), err.Error())...)
		}
	}
	if w, ok := want["requests"]; ok && !sameExact(len(requests), w) {
		p = append(p, fmt.Sprintf("requests %d, want %v", len(requests), w))
	}
	if w, ok := want["refreshCalls"]; ok && !sameExact(creds.refreshes, w) {
		p = append(p, fmt.Sprintf("refreshCalls %d, want %v", creds.refreshes, w))
	}
	if w, ok := want["tokenCalls"]; ok && !sameExact(creds.tokens, w) {
		p = append(p, fmt.Sprintf("tokenCalls %d, want %v", creds.tokens, w))
	}
	if w, ok := want["request"].(map[string]any); ok {
		if len(requests) == 0 {
			p = append(p, "no request")
		} else {
			r := requests[0]
			var body any
			_ = json.Unmarshal(r.body, &body)
			auth := strings.ReplaceAll(fmt.Sprint(w["authorization"]), "$token", token)
			got := map[string]any{"method": r.method, "path": r.path, "authorization": r.authorization, "contentType": r.contentType, "body": body}
			for key, v := range w {
				if key == "authorization" {
					v = auth
				}
				if !sameExact(got[key], v) {
					p = append(p, fmt.Sprintf("request %s = %v, want %v", key, normal(got[key]), v))
				}
			}
		}
	}
	if w, ok := want["secondAuthorization"]; ok {
		exp := strings.ReplaceAll(fmt.Sprint(w), "$refreshTo", refreshTo)
		if len(requests) < 2 || requests[1].authorization != exp {
			p = append(p, "second request did not carry the refreshed token")
		}
	}
	for _, s := range strings_(want["scrubs"]) {
		if strings.Contains(secret.Scrub("x "+s+" y"), s) {
			p = append(p, fmt.Sprintf("scrubber does not remove %q", s))
		}
	}
	return p
}
