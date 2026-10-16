package codexapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// Formatting an event, directly, through a pointer, in a struct or in a log line, shows its kind, method and size,
// never its params.
func TestEventFormattingHidesParams(t *testing.T) {
	e := Event{Method: "item/completed", Params: json.RawMessage(`{"text":"canary-said-this"}`)}
	type holder struct{ E Event }
	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("event", "e", e)
	for _, s := range []string{
		fmt.Sprint(e), fmt.Sprintf("%v %+v %#v %s %q %x", e, e, e, e, e, e), fmt.Sprintf("%+v", &e),
		fmt.Sprintf("%+v", holder{e}), e.String(), e.GoString(), logged.String(),
	} {
		if strings.Contains(s, "canary") {
			t.Errorf("params shown: %s", s)
		}
	}
	if got := e.String(); got != "codexapp.Event{notification item/completed, 27 bytes}" {
		t.Errorf("String() = %s", got)
	}
	if got := (Event{Method: "approve", Request: true}).String(); got != "codexapp.Event{server request approve, 0 bytes}" {
		t.Errorf("String() = %s", got)
	}
}
