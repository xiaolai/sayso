package codexauth

import (
	"context"
	"time"
)

// Status is the outcome of a probe. It never holds the token in a printable field.
type Status struct {
	State   State
	Info    Info
	Mode    string    // the reported sign-in mode for unsupported-auth, scrubbed and bounded
	Expires time.Time // the token's expiry when ready; zero when unknown
	Err     error     // the failure of a non-ready state, for diagnostics
	// Creds is the credentials object the probe used. It is ready to hand out the token when State is ready, and a
	// later call may succeed where this one failed (after a sign-in, or after the refresh backoff).
	Creds *Credentials
}

// Probe obtains the token once and classifies the outcome. The error is only for an invalid cfg.
func Probe(ctx context.Context, cfg Config) (*Status, error) {
	c, err := New(cfg)
	if err != nil {
		return nil, err
	}
	tok, err := c.Token(ctx)
	st := &Status{State: StateOf(err), Info: c.Info(), Err: err, Creds: c}
	if e, ok := err.(*Error); ok {
		st.Mode = e.Mode
	}
	if err == nil {
		st.Expires = tok.Expires
	}
	return st, nil
}
