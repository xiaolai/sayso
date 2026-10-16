package realtime_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/internal/fakeopenai"
	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/realtime"
)

// keptServer answers mints over HTTP/1.1 keep-alive. drop(n) says whether the n-th request (from 1, across every
// connection) is read and then met with a closed connection and no reply; idleClose closes a connection 50 ms after
// each reply, while it is idle.
type keptServer struct {
	ln        net.Listener
	mu        sync.Mutex
	requests  int
	conns     int
	drop      func(n int) bool
	idleClose bool
	// stall reads each request and then neither answers nor closes until the listener does.
	stall bool
	done  chan struct{}
}

func newKeptServer(t *testing.T, drop func(int) bool, idleClose bool) *keptServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &keptServer{ln: ln, drop: drop, idleClose: idleClose, done: make(chan struct{})}
	t.Cleanup(func() {
		close(s.done)
		_ = ln.Close()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns++
			s.mu.Unlock()
			go s.serve(c)
		}
	}()
	return s
}

func (s *keptServer) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	for {
		req, err := http.ReadRequest(r)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		s.mu.Lock()
		s.requests++
		n := s.requests
		s.mu.Unlock()
		if s.drop(n) {
			return
		}
		if s.stall {
			<-s.done
			return
		}
		body := `{"value":"ek_probe","expires_at":1900000000}`
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: "+
			itoa(len(body))+"\r\n\r\n"+body)
		if s.idleClose {
			time.Sleep(50 * time.Millisecond)
			return
		}
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func (s *keptServer) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests, s.conns
}

func twoMints(t *testing.T, s *keptServer) (first, second error) {
	c, base := fakeopenai.Client(t, "http://"+s.ln.Addr().String())
	m := &realtime.Minter{Client: c, BaseURL: base, Creds: fixedCreds{"bearer-probe"}, Timeout: 5 * time.Second, Lifetime: time.Minute}
	_, first = m.Secret(context.Background(), json.RawMessage(`{}`))
	time.Sleep(200 * time.Millisecond)
	_, second = m.Secret(context.Background(), json.RawMessage(`{}`))
	return first, second
}

// The server closes the idle connection after the first reply: net/http notices and dials again.
func TestMintAfterTheServerClosedTheIdleConnection(t *testing.T) {
	s := newKeptServer(t, func(int) bool { return false }, true)
	first, second := twoMints(t, s)
	if first != nil || second != nil {
		t.Fatalf("first %v, second %v", first, second)
	}
	if req, _ := s.counts(); req != 2 {
		t.Fatalf("requests %d, want 2", req)
	}
}

// The server reads the second mint on the kept connection and closes it without a reply: the mint is sent once
// more (spec/http-and-mint.md, lost connection), which net/http does not do for a POST.
func TestMintLostOnAKeptConnectionIsSentOnceMore(t *testing.T) {
	s := newKeptServer(t, func(n int) bool { return n == 2 }, false)
	first, second := twoMints(t, s)
	if first != nil || second != nil {
		t.Fatalf("first %v, second %v", first, second)
	}
	if req, _ := s.counts(); req != 3 {
		t.Fatalf("requests %d, want 3", req)
	}
}

// A second loss in a row is returned as network; the mint is not sent a third time.
func TestMintSecondLossIsNotSentAgain(t *testing.T) {
	s := newKeptServer(t, func(n int) bool { return n == 2 || n == 3 }, false)
	first, second := twoMints(t, s)
	if first != nil || openaihttp.Classify(second) != openaihttp.ClassNetwork {
		t.Fatalf("first %v, second %v", first, second)
	}
	if req, _ := s.counts(); req != 3 {
		t.Fatalf("requests %d, want 3", req)
	}
}

// A mint the caller's context ends while it waits is not sent again.
func TestMintNotResentAfterItsContextEnds(t *testing.T) {
	s := newKeptServer(t, func(n int) bool { return false }, false)
	s.stall = true
	c, base := fakeopenai.Client(t, "http://"+s.ln.Addr().String())
	m := &realtime.Minter{Client: c, BaseURL: base, Creds: fixedCreds{"bearer-probe"}, Timeout: 300 * time.Millisecond, Lifetime: time.Minute}
	if _, err := m.Secret(context.Background(), json.RawMessage(`{}`)); openaihttp.Classify(err) != openaihttp.ClassNetwork {
		t.Fatalf("err %v, want a network timeout", err)
	}
	if req, _ := s.counts(); req != 1 {
		t.Fatalf("requests %d, want 1", req)
	}
}
