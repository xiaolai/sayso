package codexapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strconv"

	"github.com/xiaolai/sayso/go/internal/wire"
	"github.com/xiaolai/sayso/go/secret"
)

const (
	// eventCap bounds the events held for a handler that does not keep up, the one being delivered included: beyond
	// it the client fails, as for refusals.
	eventCap = 2 * maxLine
	// eventOverhead is what each held event is charged on top of its method and params bytes, so a flood of tiny
	// events reaches eventCap too.
	eventOverhead = 64
	// maxEvents bounds the number of events held, whatever their size.
	maxEvents = 1 << 16
)

// Event is a message from the child that is not a reply: a notification, or a server request, which the client has
// already answered with an error (Request). Its contents are held where neither formatting, by any verb and through
// any wrapper, nor JSON, slog or reflection can reach them; Method, Params and Request read them. Formatting shows
// the kind, the scrubbed and escaped method and the size of the params, never the params: a notification may carry
// what the user said, or a credential.
type Event struct{ get func() eventData }

type eventData struct {
	method  string
	params  json.RawMessage
	request bool
}

func newEvent(method string, params json.RawMessage, request bool) Event {
	d := eventData{method, params, request}
	return Event{get: func() eventData { return d }}
}

func (e Event) data() eventData {
	if e.get == nil {
		return eventData{}
	}
	return e.get()
}

// Method is the method the child sent, verbatim.
func (e Event) Method() string { return e.data().method }

// Params is a copy of the params member exactly as the child wrote it, strict JSON; nil when absent (a
// "params": null is "null").
func (e Event) Params() json.RawMessage { return slices.Clone(e.data().params) }

// Request reports a server request (it had an id), which was refused before the event was queued.
func (e Event) Request() bool { return e.data().request }

// size is what the event is charged against eventCap.
func (d eventData) size() int { return eventOverhead + len(d.method) + len(d.params) }

// String shows the kind, the method scrubbed and quoted (so a control character in it cannot forge a log line or
// reach a terminal), and the size of the params.
func (e Event) String() string {
	d := e.data()
	kind := "notification"
	if d.request {
		kind = "server request"
	}
	return "codexapp.Event{" + kind + " " + strconv.Quote(secret.Scrub(d.method)) + ", " + strconv.Itoa(len(d.params)) +
		" bytes}"
}

// GoString redacts like String.
func (e Event) GoString() string { return e.String() }

// Format redacts like String for every verb.
func (e Event) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(e.String())) }

// LogValue redacts like String.
func (e Event) LogValue() slog.Value { return slog.StringValue(e.String()) }

// MarshalJSON redacts like String, so an Event inside a value a JSON log handler encodes shows no params.
func (e Event) MarshalJSON() ([]byte, error) { return json.Marshal(e.String()) }

// queueEvent hands one notification or server request, the object msg of the validated line, to the event
// goroutine. Without a handler it does nothing, so a notification is ignored as before, and nothing of it is copied.
// A notification whose method is absent or not a string is ignored too, and so is every event once the client has
// failed or Close has begun. Events held beyond eventCap or maxEvents, from a handler that does not keep up, fail the
// client.
func (k *core) queueEvent(line []byte, msg wire.Span, request bool) {
	if k.onEvent == nil {
		return
	}
	m, ok := present(line, msg, "method")
	if !ok || wire.KindOf(line, m) != '"' {
		return
	}
	var params json.RawMessage
	if p, ok := wire.Member(line, msg, "params"); ok {
		params = bytes.Clone(line[p.Start:p.End]) // the exact bytes, copied: the line is cleared once handled
	}
	d := eventData{decoded(line, m), params, request}
	k.mu.Lock()
	if k.failure != nil || k.eventsClosed {
		k.mu.Unlock()
		return
	}
	over := k.eventBytes+d.size() > eventCap || k.eventCount+1 > maxEvents
	if !over {
		k.events = append(k.events, d)
		k.eventBytes += d.size()
		k.eventCount++
	}
	k.mu.Unlock()
	if over {
		k.fail(codexError("Codex app-server sent events faster than the handler took them", nil))
		return
	}
	select {
	case k.eventWake <- struct{}{}:
	default: // a wake-up is already pending
	}
}

// eventLoop calls the handler with each queued event, one at a time, in the order the child wrote them. An event is
// admitted when it is taken from the queue, under the lock Close takes to begin: an event admitted before Close
// began is delivered, possibly while Close runs (Close does not wait for the handler, which may itself call Close);
// none is admitted after. After the child fails or exits the events already queued are still delivered. The event
// being delivered stays charged against the bounds until the handler returns.
func (k *core) eventLoop() {
	for {
		select {
		case <-k.eventWake:
		case <-k.dead:
		}
		for {
			k.mu.Lock()
			if k.eventsClosed {
				k.mu.Unlock()
				return
			}
			if len(k.events) == 0 {
				dead := k.failure != nil
				k.mu.Unlock()
				if dead {
					return
				}
				break
			}
			d := k.events[0]
			k.events[0] = eventData{}
			switch {
			case len(k.events) > 1:
				k.events = k.events[1:]
			case cap(k.events) <= 64: // drained: a small array is reused rather than reallocated per event
				k.events = k.events[:0]
			default: // drained after a burst: its array is not held while idle
				k.events = nil
			}
			k.mu.Unlock()
			if k.testEventAdmitted != nil {
				k.testEventAdmitted()
			}
			c := k.owner.Value()
			if c == nil {
				return // collected: its cleanup closes the child
			}
			k.onEvent(c, newEvent(d.method, d.params, d.request))
			runtime.KeepAlive(c) // held until the handler returns, whether or not it uses c, and no longer
			k.mu.Lock()
			if !k.eventsClosed { // Close cleared the counts with the queue
				k.eventBytes -= d.size()
				k.eventCount--
			}
			k.mu.Unlock()
		}
	}
}

// closeEvents begins Close for the events: none is admitted after it, and the queue and its counts are released.
func (k *core) closeEvents() {
	k.mu.Lock()
	k.eventsClosed = true
	k.events, k.eventBytes, k.eventCount = nil, 0, 0
	k.mu.Unlock()
	select {
	case k.eventWake <- struct{}{}: // the loop sees eventsClosed and ends
	default:
	}
}
