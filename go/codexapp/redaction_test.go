package codexapp

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/secret"
)

// readable collects every string reflection can read from v, unexported fields included, through pointers,
// interfaces, slices, arrays and maps. A func is opaque to it: reflection cannot read a closure's variables.
func readable(v reflect.Value, depth int, out *[]string) {
	if !v.IsValid() || depth > 32 {
		return
	}
	switch v.Kind() {
	case reflect.String:
		*out = append(*out, v.String())
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			readable(v.Elem(), depth+1, out)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			readable(v.Field(i), depth+1, out)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			readable(v.Index(i), depth+1, out)
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			readable(it.Key(), depth+1, out)
			readable(it.Value(), depth+1, out)
		}
	}
}

// opaque fails when fmt, through an unexported wrapper field, or reflection, field by field, shows canary, and when a
// func reached by reflection through an unexported field can be called.
func opaque(t *testing.T, name string, v any, canary string) {
	t.Helper()
	type holder struct{ inner any }
	type typed struct{ v any }
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, w := range []any{v, holder{v}, &holder{v}, typed{v}, []any{v}} {
			if s := fmt.Sprintf(verb, w); strings.Contains(s, canary) {
				t.Errorf("%s: %s shows the secret", name, verb) // the formatted text is not printed: it holds the canary
			}
		}
	}
	var found []string
	readable(reflect.ValueOf(holder{v}), 0, &found)
	for _, s := range found {
		if strings.Contains(s, canary) {
			t.Errorf("%s: reflection reads the secret", name)
		}
	}
	var call func(reflect.Value, int)
	call = func(rv reflect.Value, depth int) {
		if !rv.IsValid() || depth > 8 {
			return
		}
		switch rv.Kind() {
		case reflect.Func:
			if rv.IsNil() {
				return
			}
			func() {
				defer func() { _ = recover() }()
				for _, r := range rv.Call(nil) {
					if r.Kind() == reflect.String && strings.Contains(r.String(), canary) {
						t.Errorf("%s: a func reached by reflection returns the secret", name)
					}
				}
			}()
		case reflect.Pointer, reflect.Interface:
			if !rv.IsNil() {
				call(rv.Elem(), depth+1)
			}
		case reflect.Struct:
			for i := range rv.NumField() {
				call(rv.Field(i), depth+1)
			}
		}
	}
	call(reflect.ValueOf(holder{v}), 0)
}

// The token of an AuthStatus and the environment of a Config are out of reach of nested formatting and reflection,
// and still reach the code that needs them.
func TestSecretsAreOpaqueToFormattingAndReflection(t *testing.T) {
	secret.Register("opaque-canary-reflect-1")
	st := AuthStatus{Method: "chatgpt", HasMethod: true, token: sealed("opaque-canary-reflect-1")}
	opaque(t, "AuthStatus", st, "opaque-canary-reflect-1")
	if st.Token() != "opaque-canary-reflect-1" {
		t.Fatal("the token is unreadable through Token")
	}

	cfg := Config{Bin: "/b/codex", Env: EnvOf([]string{"SAYSO_CANARY=opaque-canary-reflect-2"}), Timeout: time.Second}
	opaque(t, "Config", cfg, "opaque-canary-reflect-2")
	opaque(t, "Env", cfg.Env, "opaque-canary-reflect-2")
	if got := cfg.Env.List(); len(got) != 1 || got[0] != "SAYSO_CANARY=opaque-canary-reflect-2" {
		t.Fatal("the environment is unreadable through List")
	}
}

// EnvOf copies its input; nil inherits, an empty list is an empty environment.
func TestEnvOf(t *testing.T) {
	kv := []string{"A=1"}
	e := EnvOf(kv)
	kv[0] = "A=2"
	if got := e.List(); got[0] != "A=1" {
		t.Errorf("EnvOf kept the caller's slice: %q", got)
	}
	e.List()[0] = "A=3"
	if got := e.List(); got[0] != "A=1" {
		t.Errorf("List exposed the held slice: %q", got)
	}
	if !EnvOf(nil).IsZero() || EnvOf(nil).List() != nil || !(Env{}).IsZero() {
		t.Error("nil does not inherit")
	}
	if empty := EnvOf([]string{}); empty.IsZero() || empty.List() == nil || len(empty.List()) != 0 {
		t.Error("an empty environment inherits")
	}
}
