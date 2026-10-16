package codexauth

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/codexapp"
)

// reach reports whether walking v by reflection, unexported fields and pointers included, finds a string containing
// needle. A func is opaque to reflection: its captured variables cannot be read.
func reach(v reflect.Value, needle string, seen map[uintptr]bool) bool {
	switch v.Kind() {
	case reflect.String:
		return strings.Contains(v.String(), needle)
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return false
		}
		if v.Kind() == reflect.Pointer {
			if seen[v.Pointer()] {
				return false
			}
			seen[v.Pointer()] = true
		}
		return reach(v.Elem(), needle, seen)
	case reflect.Struct:
		for i := range v.NumField() {
			if reach(v.Field(i), needle, seen) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if reach(v.Index(i), needle, seen) {
				return true
			}
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			if reach(it.Key(), needle, seen) || reach(it.Value(), needle, seen) {
				return true
			}
		}
	}
	return false
}

// Neither formatting a struct that holds Credentials in an unexported field nor reflection on Credentials reaches the
// environment, which lives in a closure.
func TestCredentialsEnvironmentIsBeyondReflection(t *testing.T) {
	c, err := New(Config{Source: SourceFile, Margin: time.Minute, Timeout: time.Second, Home: t.TempDir(),
		Env: codexapp.EnvOf([]string{"OPENAI_API_KEY=opaque-canary-env-6", "PATH=/usr/bin"})})
	if err != nil {
		t.Fatal(err)
	}
	type holder struct{ c Credentials }
	type ptrHolder struct{ c *Credentials }
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d"} {
		for _, v := range []any{holder{*c}, ptrHolder{c}, *c, c} {
			if s := fmt.Sprintf(verb, v); strings.Contains(s, "canary") {
				t.Errorf("%s shows the environment: %s", verb, s)
			}
		}
	}
	if reach(reflect.ValueOf(c), "canary", map[uintptr]bool{}) {
		t.Error("reflection on Credentials reaches the environment")
	}
	if got := c.env(); len(got) != 2 || got[0] != "OPENAI_API_KEY=opaque-canary-env-6" {
		t.Errorf("the environment was not kept: %d variables", len(got))
	}
	// The environment is a copy each time: a caller changing it changes nothing.
	c.env()[0] = "X=1"
	if c.env()[0] == "X=1" {
		t.Error("env returned the held slice")
	}
}

// A caller's own Config, held in an unexported field or reached by reflection, does not show its environment: Env is a
// codexapp.Env, which keeps the variables in a closure.
func TestConfigEnvironmentIsBeyondReflection(t *testing.T) {
	cfg := Config{Source: SourceFile, Env: codexapp.EnvOf([]string{"OPENAI_API_KEY=opaque-canary-env-7"})}
	type holder struct{ cfg Config }
	type ptrHolder struct{ cfg *Config }
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d"} {
		for _, v := range []any{holder{cfg}, ptrHolder{&cfg}, cfg, &cfg} {
			if s := fmt.Sprintf(verb, v); strings.Contains(s, "canary") {
				t.Errorf("%s shows the environment: %s", verb, s)
			}
		}
	}
	if reach(reflect.ValueOf(holder{cfg}), "canary", map[uintptr]bool{}) {
		t.Error("reflection on Config reaches the environment")
	}
	if got := cfg.Env.List(); len(got) != 1 {
		t.Errorf("the environment was not kept: %d variables", len(got))
	}
}
