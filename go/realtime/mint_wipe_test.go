package realtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/xiaolai/sayso/go/internal/memscan"
	"github.com/xiaolai/sayso/go/internal/prereq"
	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/realtime"
	"github.com/xiaolai/sayso/go/secret"
)

// No byte of a mint reply outlives the mint but the secret it returns: the reply is read into one buffer, which is
// cleared on every exit and whose outgrown predecessors were cleared, the value is decoded into a buffer that is
// cleared, and a refusal's body, which openaihttp.CheckResponse reads, is cleared too. The transport below the origin
// policy serves the reply from a memory scan's pages, so every other copy is one the client made; the scan counts
// each needle, and its first 16 bytes, in the process's writable memory, dropped garbage included.

// piece is a needle in a reply: as it is, kept as the secret, written as \u escapes, or registered with the
// scrubber before the mint.
type piece int

const (
	needle piece = iota
	kept
	keptEscaped
	registered
)

type mintScan struct {
	s       *memscan.Scan
	needles [][2]int
	want    []int // as Occurrences counts: each needle, then its first 16 bytes; 1 for the secret kept, else 0
}

func newMintScan(t *testing.T, room int) *mintScan {
	t.Helper()
	s, err := memscan.New(room)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	// No collection runs until the test is done: a collection can return a span of garbage to the system, which
	// zeroes it, and a copy dropped unwiped would then go unseen.
	old := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(old) })
	return &mintScan{s: s}
}

// reply builds a body in the pages from strings, its text, and pieces.
func (m *mintScan) reply(parts ...any) []byte {
	start := m.s.Mark()
	for _, p := range parts {
		switch p := p.(type) {
		case string:
			m.s.Append(p)
		case piece:
			r := m.s.Needle(40)
			m.s.Prefix(r, 16)
			m.needles = append(m.needles, r)
			k := 0
			if p == kept || p == keptEscaped {
				k = 1
			}
			m.want = append(m.want, k, k)
			b := m.s.Bytes(r)
			if p == registered {
				secret.Register(unsafe.String(unsafe.SliceData(b), len(b))) // read in place: the registry keeps no copy
			}
			if p == keptEscaped {
				for _, c := range b {
					m.s.Append(fmt.Sprintf(`\u%04x`, c))
				}
			} else {
				m.s.AppendBytes(b)
			}
		}
	}
	return m.s.Bytes([2]int{start, m.s.Mark()})
}

func (m *mintScan) check(t *testing.T) {
	t.Helper()
	found, err := m.s.Occurrences()
	if errors.Is(err, memscan.ErrUnsupported) {
		prereq.Missing(t, err.Error())
	}
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(found, m.want) {
		t.Errorf("each needle, then each one's first 16 bytes, found %v times; want %v", found, m.want)
	}
}

// failAfter reads r, then fails with err where r would end.
type failAfter struct {
	r   io.Reader
	err error
}

func (f failAfter) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		err = f.err
	}
	return n, err
}

// mintFrom mints through a transport that answers with status and body, its length declared as declared (-1 for
// none), and fails where the body ends when cut is set.
func mintFrom(t *testing.T, status int, body []byte, declared int64, cut error) (secret.ClientSecret, error) {
	t.Helper()
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var r io.Reader = bytes.NewReader(body)
		if cut != nil {
			r = failAfter{r, cut}
		}
		return &http.Response{StatusCode: status, Header: http.Header{"X-Request-Id": {"req_wipe"}}, Body: io.NopCloser(r),
			ContentLength: declared, Request: req}, nil
	})
	c, err := openaihttp.NewClient(openaihttp.WithBaseTransport(rt))
	if err != nil {
		t.Fatal(err)
	}
	m := &realtime.Minter{Client: c, BaseURL: openaihttp.APIBase, Creds: fixedCreds{"bearer-for-wipe"},
		Timeout: 5 * time.Second, Lifetime: time.Minute}
	return m.Secret(context.Background(), json.RawMessage(`{}`))
}

func TestAMintKeepsOnlyItsSecret(t *testing.T) {
	pad := strings.Repeat("x", 20<<10) // read without a declared length, the reply outgrows four buffers
	for _, c := range []struct {
		name     string
		parts    []any
		declared func(n int) int64
	}{
		{"its length declared", []any{`{"value":"`, kept, `","expires_at":1800000000,"other":"`, needle, `"}`},
			func(n int) int64 { return int64(n) }},
		{"no length declared", []any{`{"value":"`, kept, `","pad":"` + pad + `","other":["`, needle, `"],"expires_at":1}`},
			func(int) int64 { return -1 }},
		{"a declared length too short", []any{`{"value":"`, kept, `","pad":"` + pad + `","expires_at":1}`},
			func(int) int64 { return 10 }},
		{"an escaped value", []any{`{"expires_at":1,"value":"`, keptEscaped, `"}`},
			func(n int) int64 { return int64(n) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newMintScan(t, 1<<16)
			body := m.reply(c.parts...)
			sec, err := mintFrom(t, http.StatusOK, body, c.declared(len(body)), nil)
			if err != nil {
				t.Fatal(err)
			}
			m.check(t)
			if sec.Bearer() != string(m.s.Bytes(m.needles[0])) {
				t.Error("the secret is not the value sent")
			}
		})
	}
}

func TestARefusedMintLeavesNothing(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	for _, c := range []struct {
		name     string
		status   int
		parts    []any
		declared bool
		cut      error
		class    openaihttp.Class
	}{
		{"a refusal", http.StatusBadRequest, []any{`{"error":{"message":"refused `, registered, ` here","type":"t","code":"`,
			registered, `","echo":"`, needle, `"},"junk":"`, needle, `"}`}, true, nil, openaihttp.ClassInvalidRequest},
		{"a refusal of unknown length", http.StatusInternalServerError, []any{`{"error":{"message":"m","param":"`,
			registered, `"},"pad":"` + big[:30<<10] + `","n":"`, needle, `"}`}, false, nil, openaihttp.ClassServer},
		{"a malformed reply", http.StatusOK, []any{`{"value":"`, needle, `","expires_at":1`}, true, nil,
			openaihttp.ClassProtocol},
		{"a reply over 1 MiB", http.StatusOK, []any{`{"value":"`, needle, `","pad":"` + big + `"}`}, true, nil,
			openaihttp.ClassProtocol},
		{"a reply over 1 MiB of unknown length", http.StatusOK, []any{`{"value":"`, needle, `","pad":"` + big + `"}`},
			false, nil, openaihttp.ClassProtocol},
		{"a reply cut short", http.StatusOK, []any{`{"value":"`, needle, `","expires_at":1}`}, false,
			io.ErrUnexpectedEOF, openaihttp.ClassNetwork},
		{"a value too long", http.StatusOK, []any{`{"expires_at":1,"value":"`, needle, strings.Repeat("v", 4096) + `"}`},
			true, nil, openaihttp.ClassProtocol},
		{"a value of the wrong type", http.StatusOK, []any{`{"expires_at":1,"value":7,"other":"`, needle, `"}`}, true,
			nil, openaihttp.ClassProtocol},
		{"no expiry", http.StatusOK, []any{`{"value":"`, needle, `","expires_at":"soon"}`}, true, nil,
			openaihttp.ClassProtocol},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newMintScan(t, 2<<20)
			body := m.reply(c.parts...)
			declared := int64(-1)
			if c.declared {
				declared = int64(len(body))
			}
			_, err := mintFrom(t, c.status, body, declared, c.cut)
			wantClass(t, err, c.class)
			var e *openaihttp.Error
			if errors.As(err, &e) && c.status != http.StatusOK && !strings.Contains(e.Message+e.Code+e.Param, "<redacted>") {
				t.Errorf("the refusal's fields hold no scrubbed secret: %q %q %q", e.Message, e.Code, e.Param)
			}
			m.check(t)
		})
	}
}
