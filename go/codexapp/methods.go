package codexapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/xiaolai/sayso/go/secret"
)

// AuthStatus is the getAuthStatus result. The token is unexported and every formatting path redacts it.
type AuthStatus struct {
	// Method is authMethod; empty when Codex reports null (not signed in).
	Method string
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
	method, err := field[string](raw, "authMethod")
	if err != nil {
		return AuthStatus{}, bad
	}
	requires, err := optional[bool](raw, "requiresOpenaiAuth")
	if err != nil {
		return AuthStatus{}, bad
	}
	return AuthStatus{Method: method, RequiresOpenAIAuth: requires == nil || *requires, token: token}, nil
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
	PlanType             string
	OrdinaryUsageAllowed bool
	Windows              []RateWindow // the primary window, then the secondary one, when present
}

// RateWindow is one usage window of the plan.
type RateWindow struct {
	Name        string // "primary" or "secondary"
	UsedPercent float64
	WindowMins  int
	ResetsAt    time.Time // zero when not reported
}

// PlanStatus calls account/rateLimits/read and decodes the plan status.
func (c *Client) PlanStatus(ctx context.Context) (RateLimits, error) {
	type window struct {
		UsedPercent        float64 `json:"usedPercent"`
		WindowDurationMins int     `json:"windowDurationMins"`
		ResetsAt           *int64  `json:"resetsAt"`
	}
	var raw struct {
		OrdinaryUsageAllowed bool `json:"ordinaryUsageAllowed"`
		RateLimits           *struct {
			PlanType  string  `json:"planType"`
			Primary   *window `json:"primary"`
			Secondary *window `json:"secondary"`
		} `json:"rateLimits"`
	}
	if err := c.Call(ctx, "account/rateLimits/read", nil, &raw); err != nil {
		return RateLimits{}, err
	}
	out := RateLimits{OrdinaryUsageAllowed: raw.OrdinaryUsageAllowed}
	if rl := raw.RateLimits; rl != nil {
		out.PlanType = secret.Scrub(rl.PlanType)
		for _, w := range []struct {
			name string
			w    *window
		}{{"primary", rl.Primary}, {"secondary", rl.Secondary}} {
			if w.w == nil {
				continue
			}
			rw := RateWindow{Name: w.name, UsedPercent: w.w.UsedPercent, WindowMins: w.w.WindowDurationMins}
			if w.w.ResetsAt != nil {
				rw.ResetsAt = time.Unix(*w.w.ResetsAt, 0).UTC()
			}
			out.Windows = append(out.Windows, rw)
		}
	}
	return out, nil
}
