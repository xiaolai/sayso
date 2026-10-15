package codexapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/xiaolai/sayso/go/internal/wire"
	"github.com/xiaolai/sayso/go/secret"
)

// AuthStatus is the getAuthStatus result. The token is unexported, and every formatting, encoding and logging path
// redacts it and scrubs the method.
type AuthStatus struct {
	// Method is authMethod; "" when Codex reports null or nothing (HasMethod false), or reports "".
	Method string
	// HasMethod is false when authMethod is null or absent: not signed in. An empty string is a mode, and not
	// chatgpt.
	HasMethod bool
	// RequiresOpenAIAuth is requiresOpenaiAuth; false means a provider needing no OpenAI sign-in is in force.
	RequiresOpenAIAuth bool
	token              string
}

// Token returns the credential Codex sent, or "". It is already registered with the scrubber.
func (s AuthStatus) Token() string { return s.token }

func (s AuthStatus) String() string {
	return fmt.Sprintf("AuthStatus{Method:%q}", secret.Scrub(s.Method))
}

// GoString redacts like String.
func (s AuthStatus) GoString() string { return s.String() }

// Format redacts like String for every verb.
func (s AuthStatus) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.String())) }

// MarshalJSON encodes the method, scrubbed, and never the token.
func (s AuthStatus) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"method": secret.Scrub(s.Method), "requiresOpenaiAuth": s.RequiresOpenAIAuth})
}

// LogValue logs like MarshalJSON.
func (s AuthStatus) LogValue() slog.Value {
	return slog.GroupValue(slog.String("method", secret.Scrub(s.Method)), slog.Bool("requiresOpenaiAuth", s.RequiresOpenAIAuth))
}

// GetAuthStatus calls getAuthStatus. Any token in the reply is registered with the scrubber before it is returned,
// whatever was asked for.
func (c *Client) GetAuthStatus(ctx context.Context, includeToken, refreshToken bool) (AuthStatus, error) {
	var raw json.RawMessage
	params := map[string]any{"includeToken": includeToken, "refreshToken": refreshToken}
	if err := c.Call(ctx, "getAuthStatus", params, &raw); err != nil {
		return AuthStatus{}, err
	}
	bad := codexError("Codex app-server sent an unexpected getAuthStatus result", nil)
	token, err := field[string](raw, "authToken")
	if err != nil {
		return AuthStatus{}, bad
	}
	secret.Register(token) // before anything else can fail or format it
	method, err := optional[string](raw, "authMethod")
	if err != nil {
		return AuthStatus{}, bad
	}
	requires, err := optional[bool](raw, "requiresOpenaiAuth")
	if err != nil {
		return AuthStatus{}, bad
	}
	st := AuthStatus{HasMethod: method != nil, RequiresOpenAIAuth: requires == nil || *requires, token: token}
	if method != nil {
		st.Method = *method
	}
	return st, nil
}

// CredentialsStore calls config/read and returns the effective cli_auth_credentials_store; "" when absent, which
// Codex treats as "file".
func (c *Client) CredentialsStore(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := c.Call(ctx, "config/read", map[string]any{"includeLayers": false}, &raw); err != nil {
		return "", err
	}
	bad := codexError("Codex app-server sent an unexpected config/read result", nil)
	config, err := field[json.RawMessage](raw, "config")
	if err != nil {
		return "", bad
	}
	if config == nil {
		config = json.RawMessage("null")
	}
	store, err := field[string](config, "cli_auth_credentials_store")
	if err != nil {
		return "", bad
	}
	return secret.Scrub(store), nil
}

// PlanType returns account/read's account.planType, "" when absent. Only this field is decoded: the email and
// account IDs never reach a Go value. account/read may reach the network.
func (c *Client) PlanType(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := c.Call(ctx, "account/read", nil, &raw); err != nil {
		return "", err
	}
	bad := codexError("Codex app-server sent an unexpected account/read result", nil)
	account, err := field[json.RawMessage](raw, "account")
	if err != nil {
		return "", bad
	}
	if account == nil {
		return "", nil
	}
	plan, err := field[string](account, "planType")
	if err != nil {
		return "", bad
	}
	return secret.Scrub(plan), nil
}

// Version extracts the Codex version from an initialize userAgent such as "codex_cli_rs/0.162.1 (Mac OS 15.0;
// arm64)"; "" when there is none.
func Version(userAgent string) string {
	_, rest, ok := strings.Cut(userAgent, "/")
	if !ok {
		return ""
	}
	end := strings.IndexFunc(rest, func(r rune) bool { return r != '.' && (r < '0' || r > '9') })
	if end >= 0 {
		rest = rest[:end]
	}
	if rest == "" || rest[0] < '0' || rest[0] > '9' {
		return ""
	}
	return rest
}

// RateLimits is the Codex plan status from account/rateLimits/read. Only these fields are decoded; nothing that
// identifies the account reaches a Go value.
type RateLimits struct {
	PlanType string
	// OrdinaryUsageAllowed is ordinaryUsageAllowed; false when it is null or absent, which OrdinaryUsageKnown tells
	// apart from a denial.
	OrdinaryUsageAllowed bool
	OrdinaryUsageKnown   bool
	Windows              []RateWindow // the primary window, then the secondary one, when present
}

// RateWindow is one usage window of the plan.
type RateWindow struct {
	Name        string // "primary" or "secondary"
	UsedPercent float64
	WindowMins  int
	ResetsAt    time.Time // zero when not reported
}

// PlanStatus calls account/rateLimits/read and decodes the plan status, with exact keys. A window that is present
// must report usedPercent; a field of the wrong type is a codex-error.
func (c *Client) PlanStatus(ctx context.Context) (RateLimits, error) {
	var raw json.RawMessage
	if err := c.Call(ctx, "account/rateLimits/read", nil, &raw); err != nil {
		return RateLimits{}, err
	}
	out, err := decodeRateLimits(raw)
	if err != nil {
		return RateLimits{}, codexError("Codex app-server sent an unexpected account/rateLimits/read result", nil)
	}
	return out, nil
}

func decodeRateLimits(raw json.RawMessage) (RateLimits, error) {
	root, err := wire.Parse(raw)
	if err != nil {
		return RateLimits{}, err
	}
	var out RateLimits
	if root.IsNull() {
		return out, nil // result: null reads as an empty object
	}
	top, err := wire.AsObject(root)
	if err != nil {
		return RateLimits{}, err
	}
	if n, ok := top.Field("ordinaryUsageAllowed"); ok {
		if n.Kind() != 't' && n.Kind() != 'f' {
			return RateLimits{}, wire.ErrInvalid
		}
		out.OrdinaryUsageAllowed, out.OrdinaryUsageKnown = n.Kind() == 't', true
	}
	rl, ok, err := top.Object("rateLimits")
	if err != nil || !ok {
		return out, err
	}
	plan, _, err := rl.String("planType")
	if err != nil {
		return RateLimits{}, err
	}
	out.PlanType = secret.Scrub(plan)
	for _, name := range []string{"primary", "secondary"} {
		w, ok, err := rl.Object(name)
		if err != nil {
			return RateLimits{}, err
		}
		if !ok {
			continue
		}
		used, ok, err := w.Float("usedPercent")
		if err != nil || !ok {
			return RateLimits{}, wire.ErrInvalid
		}
		mins, _, err := w.Int("windowDurationMins")
		if err != nil {
			return RateLimits{}, err
		}
		rw := RateWindow{Name: name, UsedPercent: used, WindowMins: int(mins)}
		resets, ok, err := w.Int("resetsAt")
		if err != nil {
			return RateLimits{}, err
		}
		if ok {
			rw.ResetsAt = time.Unix(resets, 0).UTC()
		}
		out.Windows = append(out.Windows, rw)
	}
	return out, nil
}
