package codexapp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/xiaolai/sayso/go/internal/wire"
	"github.com/xiaolai/sayso/go/secret"
)

// eventCap bounds the events queued for a handler that does not keep up: beyond it the client fails, as for
// refusals.
const eventCap = 2 * maxLine

// Event is a message from the child that is not a reply: a notification, or a server request, which the client has
// already answered with an error (Request). Formatting shows the kind, the scrubbed method and the size of Params,
// never Params itself: a notification may carry what the user said, or a credential.
type Event struct {
	Method string
	// Params is the params member as the child sent it, strict JSON; nil when absent (a "params": null is "null").
	Params json.RawMessage
	// Request is set for a server request (it had an id), which was refused before the event was queued.
	Request bool
}

// String shows the kind, the scrubbed method and the size of Params.
func (e Event) String() string {
	kind := "notification"
	if e.Request {
		kind = "server request"
	}
	return "codexapp.Event{" + kind + " " + secret.Scrub(e.Method) + ", " + strconv.Itoa(len(e.Params)) + " bytes}"
}

// GoString redacts like String.
func (e Event) GoString() string { return e.String() }

// Format redacts like String for every verb.
func (e Event) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(e.String())) }

// LogValue redacts like String.
func (e Event) LogValue() slog.Value { return slog.StringValue(e.String()) }

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
		k.events = append(k.events, Event{Method: method, Params: params, Request: request})
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
			k.eventBytes -= len(e.Method) + len(e.Params)
			k.mu.Unlock()
			k.onEvent(e)
		}
	}
}
