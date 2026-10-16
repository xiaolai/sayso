package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/internal/fakeopenai"
	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/realtime"
	"github.com/xiaolai/sayso/go/secret"
)

// handshakeReply is what the fake server answers the upgrade request with (spec/fixtures.md).
type handshakeReply struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    *string           `json:"body"`
	Hold    bool              `json:"hold"` // keep the connection open after the reply, until the case ends
}

// replyBytes is the reply as both harnesses write it: the status line, the headers in name order, a Content-Length
// when there is a body, and Connection: close unless the reply is held.
func replyBytes(r handshakeReply) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d Fixture\r\n", r.Status)
	names := make([]string, 0, len(r.Headers))
	for name := range r.Headers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintf(&b, "%s: %s\r\n", name, r.Headers[name])
	}
	if r.Body != nil {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(*r.Body))
	}
	if !r.Hold {
		b.WriteString("Connection: close\r\n")
	}
	b.WriteString("\r\n")
	if r.Body != nil {
		b.WriteString(*r.Body)
	}
	return []byte(b.String())
}

// checkHandshake dials a fake server that reads the upgrade request and answers it with the case's reply, or never
// (silent), and compares the refusal and the request with the expectation.
func checkHandshake(t *testing.T, sc sessionCase) []string {
	if (sc.Reply == nil) != sc.Silent {
		return []string{"a handshake case has either a reply or silent: true"}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return []string{"listen: " + err.Error()}
	}
	end := make(chan struct{})
	served := make(chan struct{})
	var (
		mu                    sync.Mutex
		target, authorization string
	)
	go func() {
		defer close(served)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		mu.Lock()
		target, authorization = req.RequestURI, req.Header.Get("Authorization")
		mu.Unlock()
		if sc.Reply != nil {
			_, _ = conn.Write(replyBytes(*sc.Reply))
		}
		if sc.Reply == nil || sc.Reply.Hold {
			<-end
		}
	}()
	defer func() {
		close(end)
		_ = ln.Close()
		<-served
	}()

	client, base := fakeopenai.Client(t, "http://"+ln.Addr().String())
	timeout := time.Duration(cmp(sc.HandshakeTimeoutMs, 5000)) * time.Millisecond
	start := time.Now()
	s, err := realtime.Dial(context.Background(), realtime.DialConfig{
		Client: client, URL: realtime.WebSocketURL(base, "m"),
		Secret:           secret.NewClientSecret("ek_conformance", time.Unix(1800000000, 0)),
		HandshakeTimeout: timeout, ReadLimit: 1 << 20,
	})
	elapsed := time.Since(start)
	if err == nil {
		_ = s.Close()
		return []string{"the handshake opened a session"}
	}
	want := sc.Expect
	var p []string
	var e *openaihttp.Error
	if !errors.As(err, &e) {
		return []string{fmt.Sprintf("not a classified error: %v", err)}
	}
	for key, got := range map[string]any{"class": string(e.Class), "status": e.Status, "requestId": e.RequestID, "code": e.Code} {
		if w, ok := want[key]; ok && !sameExact(got, w) {
			p = append(p, fmt.Sprintf("%s %v, want %v", key, got, w))
		}
	}
	if w, ok := want["withinMs"].(json.Number); ok {
		ms, err := w.Int64()
		if limit := time.Duration(ms) * time.Millisecond; err != nil || elapsed > limit {
			p = append(p, fmt.Sprintf("took %v, limit %v ms", elapsed, w))
		}
	}
	if w, ok := want["request"].(map[string]any); ok {
		mu.Lock()
		got := map[string]any{"target": target, "authorization": authorization}
		mu.Unlock()
		for key, v := range w {
			if !sameExact(got[key], v) {
				p = append(p, fmt.Sprintf("request %s %q, want %v", key, got[key], v))
			}
		}
	}
	return append(p, leaks(strings_(want["notInDescription"]), err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err))...)
}
