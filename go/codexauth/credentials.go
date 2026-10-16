package codexauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xiaolai/sayso/go/codexapp"
	"github.com/xiaolai/sayso/go/secret"
)

// Source chooses where the token comes from.
type Source string

// Sources.
const (
	SourceAuto  Source = "auto"  // Codex when it resolves, else auth.json
	SourceCodex Source = "codex" // Codex only
	SourceFile  Source = "file"  // auth.json only
)

const (
	backoffStart = 60 * time.Second
	backoffMax   = 15 * time.Minute
)

// Config describes how to obtain the token.
type Config struct {
	Source    Source // "" means SourceAuto
	Bin       string // an explicitly chosen executable (a path, or a name looked up in PATH); "" resolves
	CodexHome string // "" means CODEX_HOME from Env, else unset
	Home      string // the user's home folder; "" means os.UserHomeDir
	// ConfineCodexHome, when set, is the folder a CODEX_HOME must resolve inside for auth.json to be read (a
	// sandboxed app confines it to the home folder). "" confines nothing; ownership is checked either way.
	ConfineCodexHome string
	// Env is the child's environment; the zero Env inherits os.Environ(). codexapp.Env never shows a variable, under
	// any verb or by reflection, so a Config held in an unexported field cannot leak it.
	Env codexapp.Env
	// Candidates are the folders searched after PATH; nil means codexapp.Candidates(Home).
	Candidates []string
	// Margin is how much lifetime a token must have left; below it, it is refreshed before use. Required.
	Margin time.Duration
	// Timeout bounds every app-server call. Required.
	Timeout time.Duration
	Now     func() time.Time // nil means time.Now
	Logger  *slog.Logger     // nil discards
	// ClientName is sent to Codex as clientInfo.name; "" means "sayso".
	ClientName string
	// Guard, if set, runs before an executable is started or auth.json is read, with its path; an error refuses.
	// A consumer uses it to keep unit tests away from the real Codex and the real sign-in.
	Guard func(path string) error
	// OnFileRetry, if set, runs before the one re-read of auth.json after a parse failure. For tests.
	OnFileRetry func()
}

// Info describes the latest fetch.
type Info struct {
	Bin          string // the resolved executable; "" when none
	MissingBin   string // the explicit executable that did not resolve
	Source       Source // SourceCodex or SourceFile: where the latest token came from, or was looked for
	CodexHome    string // the CODEX_HOME in force; "" when unset
	CodexVersion string // "" when unknown
}

// Credentials hands out the bearer and owns the refresh policy: single-flight, a refresh succeeds when the token it
// returns is fresh, and a failure backs off from 60 s, doubling to 15 min, without a permanent budget. It is safe for
// concurrent use and lives as long as the caller wants; Codex runs only during a fetch or a refresh.
type Credentials struct {
	// cfg sits behind a pointer, so formatting a struct that holds Credentials in an unexported field (which fmt
	// prints by reflection, without the redacting methods) shows an address. Its Env is the zero Env: the environment is in env.
	cfg *Config // never changed after New
	// env returns a copy of the child's environment. The variables live in a closure, which fmt prints as an address
	// and reflection cannot read, so neither formatting nor reflection on Credentials reaches them.
	env  func() []string
	path string // PATH of the inherited environment

	lock chan struct{} // held across a fetch or refresh
	// The mutable state sits behind a pointer, so formatting a copy of the value (the redacting methods take the
	// value) reads none of it and cannot race with a fetch.
	*state
	// refreshes counts successful refreshes. A caller that refused a token while a refresh was running takes that
	// refresh's result instead of starting another, even when Codex returned the same bearer.
	refreshes *atomic.Int64
	// refresh asks for a refreshed token; refreshCodex, replaced only by this package's tests.
	refresh func(context.Context) (secret.Token, error)
}

// state is what a fetch or refresh changes; read and written only with the lock held.
type state struct {
	cur         *secret.Token
	refreshable bool // cur came from Codex
	// refused is the bearer a provider last refused and no refresh has renewed since: it is not handed out again
	// until a refresh succeeds or the file holds another token.
	refused string
	backoff time.Duration
	retryAt time.Time
	info    Info
}

// New validates cfg and returns credentials that fetch on first use.
func New(cfg Config) (*Credentials, error) {
	if cfg.Margin <= 0 || cfg.Timeout <= 0 {
		return nil, errors.New("codexauth: Margin and Timeout are required")
	}
	switch cfg.Source {
	case "":
		cfg.Source = SourceAuto
	case SourceAuto, SourceCodex, SourceFile:
	default:
		return nil, fmt.Errorf("codexauth: unknown source %q", cfg.Source)
	}
	// Copies: a caller changing its slices later must not change the child's environment or the executable lookup,
	// nor race with a fetch. nil stays nil, so its default still applies.
	kv := cfg.Env.List()
	cfg.Candidates = slices.Clone(cfg.Candidates)
	if cfg.Env.IsZero() {
		kv = os.Environ()
	}
	if cfg.Home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("codexauth: locate the home folder: %w", err)
		}
		cfg.Home = h
	}
	path := ""
	for _, v := range kv {
		if name, value, _ := strings.Cut(v, "="); name == "CODEX_HOME" && cfg.CodexHome == "" {
			cfg.CodexHome = value
		} else if name == "PATH" {
			path = value
		}
	}
	if cfg.Candidates == nil {
		cfg.Candidates = codexapp.Candidates(cfg.Home)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	held := kv
	cfg.Env = codexapp.Env{}
	env := func() []string { return slices.Clone(held) }
	c := &Credentials{cfg: &cfg, env: env, path: path, lock: make(chan struct{}, 1), state: &state{}, refreshes: new(atomic.Int64)}
	c.refresh = c.refreshCodex
	return c, nil
}

// acquire takes the lock unless ctx has ended. select picks at random between ready cases, so a cancelled caller is
// checked before and after: it never runs, as in Swift.
func (c *Credentials) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case c.lock <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-c.lock
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Credentials) release() { <-c.lock }

// Info returns what the latest fetch found.
func (c *Credentials) Info() Info {
	if c.acquire(context.Background()) != nil {
		return Info{}
	}
	defer c.release()
	return c.info
}

// fresh reports whether t has at least the margin left; a token of unknown expiry is fresh, and the provider decides.
func (c *Credentials) fresh(t secret.Token) bool {
	return t.Expires.IsZero() || t.Expires.Sub(c.cfg.Now()) >= c.cfg.Margin
}

// usable reports whether t may be handed out: fresh, and not the bearer a provider refused.
func (c *Credentials) usable(t secret.Token) bool {
	return c.fresh(t) && (c.refused == "" || t.Bearer() != c.refused)
}

// Token returns a fresh bearer, fetching on first use and refreshing when less than the margin remains. A token from
// auth.json is re-read rather than refreshed. The error is an *Error carrying the state.
func (c *Credentials) Token(ctx context.Context) (secret.Token, error) {
	if err := c.acquire(ctx); err != nil {
		return secret.Token{}, err
	}
	defer c.release()
	if c.cur == nil || (!c.refreshable && !c.usable(*c.cur)) {
		if err := c.fetchLocked(ctx); err != nil {
			return secret.Token{}, err
		}
	}
	if c.usable(*c.cur) {
		return *c.cur, nil
	}
	return c.refreshLocked(ctx)
}

// RefreshAfter is called when a request made with stale was refused as auth-expired. When stale was already
// replaced by a fresh token, or a refresh completed while this caller waited, that token is returned without another
// refresh; otherwise a refresh runs, within the backoff. Until a refresh succeeds, Token does not hand out stale.
func (c *Credentials) RefreshAfter(ctx context.Context, stale secret.Token) (secret.Token, error) {
	seen := c.refreshes.Load()
	if err := c.acquire(ctx); err != nil {
		return secret.Token{}, err
	}
	defer c.release()
	if c.cur != nil && c.refreshes.Load() != seen && c.usable(*c.cur) {
		return *c.cur, nil // a refresh ran while this caller waited, and its token has not been refused since
	}
	if c.cur != nil && c.cur.Bearer() != stale.Bearer() {
		if c.usable(*c.cur) {
			return *c.cur, nil
		}
		// stale is an older token. The current one is refused or within the margin already; recording stale would
		// forget that the current one was refused.
	} else {
		c.refused = stale.Bearer()
	}
	if c.cur == nil || !c.refreshable {
		if err := c.fetchLocked(ctx); err != nil {
			return secret.Token{}, err
		}
		if c.usable(*c.cur) {
			return *c.cur, nil
		}
		if !c.refreshable {
			return secret.Token{}, fail(StateExpired, "auth.json holds the refused token, and the file source cannot refresh", nil)
		}
	}
	return c.refreshLocked(ctx)
}

// refreshLocked asks Codex for a refreshed token, within the backoff. The caller holds the lock.
func (c *Credentials) refreshLocked(ctx context.Context) (secret.Token, error) {
	if !c.refreshable {
		return secret.Token{}, fail(StateExpired, "the token in auth.json is expiring, and the file source cannot refresh", nil)
	}
	if c.cfg.Now().Before(c.retryAt) {
		return secret.Token{}, fail(StateExpired, "the last refresh failed; the next attempt waits for the backoff", nil)
	}
	tok, err := c.refresh(ctx)
	if err == nil && !c.fresh(tok) {
		err = errors.New("Codex returned a token that is still within the margin")
	}
	if err != nil {
		if ctx.Err() != nil {
			// The caller gave up; Codex did not fail, so no backoff is spent.
			return secret.Token{}, fail(StateExpired, "the refresh was cancelled", err)
		}
		if c.backoff == 0 {
			c.backoff = backoffStart
		} else {
			c.backoff = min(2*c.backoff, backoffMax)
		}
		c.retryAt = c.cfg.Now().Add(c.backoff) // from the failure, so a slow attempt does not use up the wait
		var e *Error
		if errors.As(err, &e) && signInChanged(e.State) {
			return secret.Token{}, e // the refresh reply named another sign-in: classified as in step 3
		}
		return secret.Token{}, fail(StateExpired, "the refresh did not renew the sign-in", err)
	}
	c.backoff, c.retryAt, c.refused = 0, time.Time{}, ""
	c.cur = &tok
	c.refreshes.Add(1)
	return tok, nil
}

// signInChanged reports a state a refresh reply is classified as when the sign-in itself changed (spec/credentials.md,
// sequence step 4): the caller must see it, not expired.
func signInChanged(s State) bool {
	switch s {
	case StateNotSignedIn, StateAPIKey, StateUnsupported, StateProviderWithoutOpenAIAuth:
		return true
	}
	return false
}

// fetchLocked obtains a token from the source and installs it. The caller holds the lock.
func (c *Credentials) fetchLocked(ctx context.Context) error {
	tok, refreshable, e := c.fetch(ctx)
	if e != nil {
		e.CodexVersion = c.info.CodexVersion
		return e
	}
	c.cur, c.refreshable = &tok, refreshable
	return nil
}

func (c *Credentials) fetch(ctx context.Context) (secret.Token, bool, *Error) {
	c.info = Info{CodexHome: c.cfg.CodexHome}
	if c.cfg.Source == SourceFile {
		tok, e := c.fileToken(false)
		return tok, false, e
	}
	bin, err := codexapp.Resolve(c.cfg.Bin, c.path, c.cfg.Candidates)
	if err != nil {
		if c.cfg.Bin != "" || c.cfg.Source == SourceCodex {
			c.info.MissingBin = c.cfg.Bin
			return secret.Token{}, false, fail(StateNoCodex, "no Codex executable resolved", err)
		}
		tok, e := c.fileToken(true)
		return tok, false, e
	}
	c.info.Bin = bin
	return c.codexToken(ctx, bin)
}

// fileToken reads auth.json. fallback marks the file source as the fallback for a missing executable: a missing
// file is then no-codex.
func (c *Credentials) fileToken(fallback bool) (secret.Token, *Error) {
	c.info.Source = SourceFile
	loc, e := authFilePath(c.cfg.CodexHome, c.cfg.Home, c.cfg.ConfineCodexHome)
	if e != nil {
		return secret.Token{}, c.fallbackState(e, fallback)
	}
	defer loc.close()
	if c.cfg.Guard != nil {
		if err := c.cfg.Guard(loc.path); err != nil {
			return secret.Token{}, fail(StateCodexError, "reading auth.json was refused", err)
		}
	}
	tok, e := readAuthFile(loc, c.cfg.OnFileRetry)
	if e != nil {
		return secret.Token{}, c.fallbackState(e, fallback)
	}
	return tok, nil
}

// fallbackState turns a missing or unusable auth.json into no-codex when the file was only the fallback for a
// missing executable: with no usable file either, the fix is to install Codex. A sign-in state stays as it is.
func (c *Credentials) fallbackState(e *Error, fallback bool) *Error {
	if fallback && (e.State == StateNoSignInFile || e.State == StateCodexError) {
		return fail(StateNoCodex, "no Codex executable resolved and auth.json is missing or unusable", e)
	}
	return e
}

func (c *Credentials) start(ctx context.Context, bin string) (*codexapp.Client, *Error) {
	cl, err := codexapp.Start(ctx, codexapp.Config{
		Bin: bin, CodexHome: c.cfg.CodexHome, Env: codexapp.EnvOf(c.env()), Timeout: c.cfg.Timeout, Logger: c.cfg.Logger,
		ClientName: c.cfg.ClientName, Guard: c.cfg.Guard,
	})
	if err != nil {
		return nil, fail(StateCodexError, "Codex app-server did not start", err)
	}
	c.info.CodexVersion = codexapp.Version(cl.UserAgent())
	if v := c.info.CodexVersion; v != "" && codexapp.VersionLess(v, MinCodexVersion) {
		_ = cl.Close()
		e := fail(StateCodexTooOld, "Codex is older than "+MinCodexVersion, nil)
		e.CodexVersion = v
		return nil, e
	}
	return cl, nil
}

// codexToken asks a fresh app-server child: metadata first, and the token only for a ChatGPT sign-in, so an API key
// is never requested.
func (c *Credentials) codexToken(ctx context.Context, bin string) (secret.Token, bool, *Error) {
	c.info.Source = SourceCodex
	cl, e := c.start(ctx, bin)
	if e != nil {
		return secret.Token{}, false, e
	}
	defer func() { _ = cl.Close() }()
	meta, err := cl.GetAuthStatus(ctx, false, false)
	if methodMissing(err) {
		_ = cl.Close()
		return c.storeFallback(ctx, bin)
	}
	if err != nil {
		return secret.Token{}, false, fail(StateCodexError, "Codex app-server getAuthStatus failed", err)
	}
	if !meta.RequiresOpenAIAuth {
		return secret.Token{}, false, fail(StateProviderWithoutOpenAIAuth, "the provider in force needs no OpenAI sign-in", nil)
	}
	if e := classifyStatus(meta); e != nil {
		return secret.Token{}, false, e
	}
	full, err := cl.GetAuthStatus(ctx, true, false)
	if methodMissing(err) {
		_ = cl.Close()
		return c.storeFallback(ctx, bin)
	}
	tok, e := tokenFrom(full, err)
	return tok, e == nil, e
}

// methodMissing reports Codex answering getAuthStatus with -32601: the method was removed (spec/credentials.md).
func methodMissing(err error) bool {
	var rpc *codexapp.RPCError
	return errors.As(err, &rpc) && rpc.Code == codexapp.CodeMethodNotFound
}

// storeFallback runs when Codex lacks getAuthStatus: a fresh child's config/read decides whether auth.json is
// Codex's authority. A running app-server keeps the store it started with, so the answer comes from a new child.
func (c *Credentials) storeFallback(ctx context.Context, bin string) (secret.Token, bool, *Error) {
	cl, e := c.start(ctx, bin)
	if e != nil {
		return secret.Token{}, false, e
	}
	store, err := cl.CredentialsStore(ctx)
	_ = cl.Close()
	var rpc *codexapp.RPCError
	if errors.As(err, &rpc) && rpc.Code == codexapp.CodeMethodNotFound {
		// Without config/read the store cannot be confirmed to be the file, so the file is not Codex's authority.
		return secret.Token{}, false, fail(StateAppServerAuthUnavailable, "Codex lacks getAuthStatus and config/read", err)
	}
	if err != nil {
		return secret.Token{}, false, fail(StateCodexError, "Codex app-server config/read failed", err)
	}
	if store != "" && store != "file" {
		e := fail(StateAppServerAuthUnavailable, "Codex lacks getAuthStatus and keeps its sign-in in "+store, nil)
		return secret.Token{}, false, e
	}
	tok, e := c.fileToken(false)
	return tok, false, e
}

// refreshCodex asks a fresh child to refresh the sign-in.
func (c *Credentials) refreshCodex(ctx context.Context) (secret.Token, error) {
	bin, err := codexapp.Resolve(c.cfg.Bin, c.path, c.cfg.Candidates)
	if err != nil {
		return secret.Token{}, err
	}
	cl, e := c.start(ctx, bin)
	if e != nil {
		return secret.Token{}, e
	}
	defer func() { _ = cl.Close() }()
	full, err := cl.GetAuthStatus(ctx, true, true)
	if methodMissing(err) {
		// Codex was updated to one without getAuthStatus since the token was fetched: the same config/read rule
		// decides, and a file token cannot be refreshed again. The caller holds the lock.
		_ = cl.Close()
		tok, _, e := c.storeFallback(ctx, bin)
		if e != nil {
			return secret.Token{}, e
		}
		c.refreshable = false
		return tok, nil
	}
	tok, e := tokenFrom(full, err)
	if e != nil {
		return secret.Token{}, e
	}
	return tok, nil
}

// tokenFrom checks the provider and the method again on a token reply: a change is classified like the metadata
// reply.
func tokenFrom(full codexapp.AuthStatus, err error) (secret.Token, *Error) {
	if err != nil {
		return secret.Token{}, fail(StateCodexError, "Codex app-server getAuthStatus failed", err)
	}
	if !full.RequiresOpenAIAuth {
		return secret.Token{}, fail(StateProviderWithoutOpenAIAuth, "the provider in force needs no OpenAI sign-in", nil)
	}
	if e := classifyStatus(full); e != nil {
		return secret.Token{}, e
	}
	if full.Token() == "" {
		return secret.Token{}, fail(StateCodexError, "Codex reported a ChatGPT sign-in without a token", nil)
	}
	return newToken(full.Token(), "codex"), nil
}

// String names the source only, never the cached token or the environment. The formatting methods are on the value
// so a copy (fmt.Sprintf("%+v", *c)) redacts too; they read only cfg, which never changes.
func (c Credentials) String() string { return "codexauth.Credentials{" + string(c.cfg.Source) + "}" }

// GoString redacts like String.
func (c Credentials) GoString() string { return c.String() }

// Format redacts like String for every verb.
func (c Credentials) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(c.String())) }

// String names the source only: Env holds the inherited environment, which may hold credentials.
func (cfg Config) String() string { return "codexauth.Config{" + string(cfg.Source) + "}" }

// GoString redacts like String.
func (cfg Config) GoString() string { return cfg.String() }

// Format redacts like String for every verb.
func (cfg Config) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(cfg.String())) }
