package codexapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

// reachable reports whether walking v by reflection, unexported fields and pointers included, finds a string or
// byte slice containing needle. A func is opaque to reflection: what it captured cannot be read.
func reachable(v reflect.Value, needle string) bool {
	switch v.Kind() {
	case reflect.String:
		return strings.Contains(v.String(), needle)
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return strings.Contains(string(v.Bytes()), needle)
		}
		for i := range v.Len() {
			if reachable(v.Index(i), needle) {
				return true
			}
		}
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil() && reachable(v.Elem(), needle)
	case reflect.Struct:
		for i := range v.NumField() {
			if reachable(v.Field(i), needle) {
				return true
			}
		}
	}
	return false
}

// Formatting an event, by any verb, directly, through a pointer, in an exported or unexported wrapper field, in a
// text or JSON log line (nested in a slice or a struct too), or as JSON, shows its kind, method and size, never its
// params; nor can reflection reach them.
func TestEventFormattingHidesParams(t *testing.T) {
	e := newEvent("item/completed", json.RawMessage(`{"text":"canary-said-this"}`), false)
	type holder struct{ E Event }
	type hidden struct{ e Event }
	var h any = hidden{e} // as any, so vet lets %s take it, as a caller's code can
	var text, structured bytes.Buffer
	slog.New(slog.NewTextHandler(&text, nil)).Info("event", "e", e, "list", []Event{e}, "held", hidden{e})
	slog.New(slog.NewJSONHandler(&structured, nil)).Info("event", "e", e, "list", []Event{e}, "held", holder{e},
		"map", map[string]Event{"k": e})
	marshalled, err := json.Marshal(map[string]any{"e": e, "list": []Event{e}, "held": holder{e}})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		fmt.Sprint(e), fmt.Sprintf("%v %+v %#v %s %q %x %d", e, e, e, e, e, e, e), fmt.Sprintf("%+v", &e),
		fmt.Sprintf("%+v", holder{e}), fmt.Sprintf("%s %+v %#v %v %x", h, h, h, &hidden{e}, h),
		fmt.Sprintf("%v", []Event{e}), e.String(), e.GoString(), text.String(), structured.String(), string(marshalled),
	} {
		if strings.Contains(s, "canary") || strings.Contains(s, "said-this") {
			t.Errorf("params shown: %s", s)
		}
	}
	if reachable(reflect.ValueOf(hidden{e}), "canary") {
		t.Error("reflection reaches the params")
	}
	if got := e.String(); got != `codexapp.Event{notification "item/completed", 27 bytes}` {
		t.Errorf("String() = %s", got)
	}
	if got := newEvent("approve", nil, true).String(); got != `codexapp.Event{server request "approve", 0 bytes}` {
		t.Errorf("String() = %s", got)
	}
	// The accessors read what was held, and Params is a copy.
	if e.Method() != "item/completed" || e.Request() || string(e.Params()) != `{"text":"canary-said-this"}` {
		t.Errorf("accessors: %q %v %q", e.Method(), e.Request(), e.Params())
	}
	e.Params()[0] = 'X'
	if e.Params()[0] != '{' {
		t.Error("Params returned the held bytes")
	}
	if (Event{}).Method() != "" || (Event{}).Params() != nil {
		t.Error("the zero Event is not empty")
	}
}

// A method the child controls cannot forge a log line or reach a terminal: control characters are escaped.
func TestEventMethodIsEscaped(t *testing.T) {
	for _, m := range []string{"a\nlevel=ERROR msg=forged", "a\rb", "a\x1b[31mred", "a b", "a‮b"} {
		e := newEvent(m, nil, false)
		var text bytes.Buffer
		slog.New(slog.NewTextHandler(&text, nil)).Info("event", "e", e)
		for _, s := range []string{e.String(), fmt.Sprintf("%v", e), text.String()} {
			if strings.ContainsAny(strings.TrimSuffix(s, "\n"), "\n\r\x1b ‮") {
				t.Errorf("%q formats with a control character: %q", m, s)
			}
		}
	}
}
