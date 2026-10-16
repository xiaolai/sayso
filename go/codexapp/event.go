package codexapp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	"github.com/xiaolai/sayso/go/internal/wire"
	"github.com/xiaolai/sayso/go/secret"
)

// eventCap bounds the events queued for a handler that does not keep up: beyond it the client fails, as for
// refusals.
const eventCap = 2 * maxLine

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

// queueEvent hands one notification or server request to the event goroutine. Without a handler it does nothing, so
// a notification is ignored as before. A notification whose method is absent or not a string is ignored too. Events
// beyond eventCap, from a handler that does not keep up, fail the client.
func (k *core) queueEvent(m wire.Object, request bool) {
	if k.onEvent == nil {
		return
	}
	method, ok, err := m.String("method")
	if err != nil || !ok {
		return
	}
	var params json.RawMessage
	if p, present := m["params"]; present {
		params = json.RawMessage(p.Raw()) // a copy: Raw returns a new string
	}
	size := len(method) + len(params)
	k.mu.Lock()
	if k.failure != nil { // the client is done: nothing more is delivered
		k.mu.Unlock()
		return
	}
	over := k.eventBytes+size > eventCap
	if !over {
		k.events = append(k.events, newEvent(method, params, request))
		k.eventBytes += size
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

// eventLoop calls the handler with each queued event, one at a time, in the order the child wrote them. After the
// child fails or exits it still delivers what was queued; Close stops it before the next event.
func (k *core) eventLoop() {
	for {
		select {
		case <-k.eventWake:
		case <-k.dead:
		}
		for {
			if k.closing.Load() {
				return
			}
			k.mu.Lock()
			if len(k.events) == 0 {
				dead := k.failure != nil
				k.mu.Unlock()
				if dead {
					return
				}
				break
			}
			e := k.events[0]
			k.events[0] = Event{}
			k.events = k.events[1:]
			d := e.data()
			k.eventBytes -= len(d.method) + len(d.params)
			k.mu.Unlock()
			k.onEvent(e)
		}
	}
}
