# Credentials (`AUTH`)

Component `credentials`. Product `CodexAuth` (Swift, macOS only), package `codexauth` and `codexapp` (Go).
Fixtures: `fixtures/auth-file/`, `fixtures/app-server/`.

The ChatGPT access token Codex holds for this machine is asked of Codex first, through `codex app-server`, and read
from `auth.json` only when no Codex runs or Codex says the store is a file. Only a ChatGPT sign-in is accepted.

## States (`AUTH-07`)

Exactly one applies after a probe.

| State | Meaning |
|---|---|
| `ready` | a usable ChatGPT token, fresh beyond the caller's margin or of unknown expiry |
| `no-codex` | no Codex executable resolved and no usable `auth.json` either, or an explicitly named executable that does not resolve |
| `codex-too-old` | Codex reports a version below the minimum, `0.161.0` |
| `not-signed-in` | Codex reports no sign-in (`authMethod: null`) |
| `no-sign-in-file` | the file source was chosen and `auth.json` does not exist; the sign-in may be in the keychain |
| `api-key` | Codex is signed in with an API key |
| `unsupported-auth` | any other sign-in mode; the mode is reported, at most 64 printable characters, scrubbed |
| `expired` | the token is within the caller's margin of expiry and a refresh did not renew it |
| `codex-error` | Codex failed: did not start, crashed, timed out, answered malformed or with an error, or the file is unusable |
| `app-server-auth-unavailable` | Codex lacks `getAuthStatus` and its effective store is not a file |
| `provider-without-openai-auth` | Codex reports `requiresOpenaiAuth: false`: a provider needing no OpenAI sign-in is in force |

Apps map states to their own text; this package has no user-facing strings.

A fetch or refresh the caller cancels while it runs keeps the state it reached, but its error has the class
`cancelled`, not that state's class: an interrupt is never reported as a credential failure.

## Executable (`AUTH-01`)

In order: an explicit path from the caller; then `PATH`; then the GUI candidates, because an app launched from the
Finder has no login-shell `PATH`: `/opt/homebrew/bin`, `/usr/local/bin`, then `~/.local/bin`, `~/.npm-global/bin`,
`~/.volta/bin`, `~/.bun/bin`, `~/Library/pnpm`, then every `~/.nvm/versions/node/*/bin`, newest version first by
numeric comparison (a link named like a version is not a version folder). The result is absolute, a regular file,
and executable by this process (`access(X_OK)`: an execute bit for other users only does not stop the search). A
path holding a NUL matches nothing. A file every user can change is not used: one writable by all, or reached
through a folder writable by all (links followed as the system follows them, each link's own folder included; a
sticky folder owned by the user or root, holding an entry owned by one of them, does not count). A search skips it;
an explicit path to it is `no-codex`. This is world-write hardening, not a judgement of the installation's owners:
group write (a multi-user Homebrew prefix), ACLs, owners and the interpreter a script names (`node`) are not checked. An explicit path that does not resolve is `no-codex`, never a fall-through to the
list. Version components compare as integers; a non-numeric one is 0 and one too large for a 64-bit integer saturates
(Go's `strconv.Atoi`).

## The app-server source (`AUTH-01`, `AUTH-03`)

- **Child.** `<codex> -c model_provider="openai" app-server --listen stdio://`. The provider override makes Codex
  answer for its OpenAI sign-in even when the user's configured provider needs none. It starts in the caller's
  working directory unless the caller names another, which must be an absolute path to an existing folder; any other
  is refused before anything starts, with an error that does not quote the path.
- **Environment.** Inherited, minus these names compared case-insensitively: `OPENAI_API_KEY`, `CODEX_API_KEY`,
  `CODEX_ACCESS_TOKEN`, `CODEX_APP_SERVER_LOGIN_CLIENT_ID`, `CODEX_REFRESH_TOKEN_URL_OVERRIDE`,
  `CODEX_REVOKE_TOKEN_URL_OVERRIDE`. `CODEX_HOME` is set when the caller configured one (an empty choice is no
  choice: the inherited `CODEX_HOME` applies, to the child and the file alike). `PATH` is the
  executable's folder, then `/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin`, then the inherited `PATH`, so an
  npm-installed `codex` (`#!/usr/bin/env node`) finds its interpreter. Nothing else is added. A name holding `=`
  or a NUL, or a value (`CODEX_HOME` included) holding a NUL, cannot be passed on as given: it is `codex-error`
  before anything starts.
- **Lifetime.** Started on demand for one fetch or refresh, then closed; the token is cached in memory, not the
  child. The child starts in its own process group. Close: stdin closed, `SIGTERM` to the group, a 2 s grace,
  `SIGKILL` to the group unless the child was reaped and no other member of its group remains; close returns within
  grace + 1 s even when a descendant outside the group still holds the pipes (our ends are closed and the readers
  abandoned). A client released without a close ends its child the same way. A child that exits stays unreaped
  until close, so its process-group ID cannot be reused by another process while close may still signal it; close
  reaps it last. A host that ignores `SIGCHLD` has the kernel reap the child at exit, after which its ID may name
  another process: close signals the group only while the child is still its own, alive or unreaped, and once the
  kernel has reaped it the group is not signalled (a descendant left in it is not ended). An implementation that
  cannot keep the child unreaped on a platform does not support that platform: the Go client builds on macOS and
  Linux only.
- **Messages.** Newline-delimited JSON-RPC, UTF-8 without a byte-order mark; a last line ended by end of file rather
  than a newline is still a line. Only JSON's own whitespace (space, tab, line feed, carriage return) is trimmed
  around a line. A line longer than 4 MiB, or a line that is not strict JSON (one value, valid UTF-8, no duplicate
  key, nested at most 64 levels, no number beyond the range of a 64-bit float such as `1e999`), is `codex-error`;
  so is a reply with neither `result` nor `error`, while
  `result: null` reads as an empty object. Keys are matched exactly (`AUTHTOKEN` is not `authToken`), and a present
  field of the wrong type is `codex-error` (an error `code` is a signed 64-bit integer). A message is classified
  before its reply fields are read: one without `id` is a notification and is ignored whatever else it holds; one
  with both `id` and a `method` (a string, even `""`), such as an approval, is a server request, answered with a
  JSON-RPC error and never acted on. Every message the client sends, requests and these answers alike, goes through
  one ordered queue that a writer of its own drains, so the reader never waits on the child's input. The order is
  the order of admission to that queue: a request is admitted when its call queues it, and an answer when the reader
  reads the server request it answers (its place is taken as soon as the line is parsed, before the answer is
  encoded; the writer waits at that place until the answer's bytes are in). So the child reads each answer after every request queued before the reader
  read that server request (a request already queued goes first, even if its write has not started) and before
  every request queued after. A request that cannot get its turn within its
  timeout is taken off the queue unwritten; one whose write has started is ended by its timeout. Each answer gets
  the per-call timeout from when its write starts; an answer that cannot be written is `codex-error` and fails the
  client, since the child would wait for it and every later request would be written ahead of it. Answers queued
  beyond 8 MiB (a child sending requests without reading the answers) are `codex-error`. Close waits up to 100 ms
  for the messages queued before it before it closes the child's input, so a request sent before close is
  answered. Any other message is a reply, matched to a call by its integer `id`. Once the client has failed, a
  reply read afterwards is not handed to its call, and nothing more is read from the child; the queue is dropped,
  nothing more is written, and every call still queued fails with the failure.
- **Events.** A caller may give the client an event handler. It then receives every notification whose `method` is
  a string, and every server request after its refusal was queued, as an event: the method, the `params` member
  exactly as the child wrote it, byte for byte (absent when the message had none; `null` when it was null), and
  whether it was a server request. Events are delivered one at a time, in the order the child wrote them, from a
  thread or goroutine of the client's own, so the reader never waits for the handler; a reply may reach its caller
  before the handler has seen the events written before it. Each event held, the one being handled included, is
  charged 64 bytes plus the bytes of its method and params; more than 8 MiB charged, or more than 65,536 events
  held, is `codex-error` (a handler that does not keep up), which fails the client. Once the client has failed
  nothing more is queued, but what was queued is still delivered. Close begins under the same lock the event
  thread admits each event under: once it has begun no event is admitted and the queue is released; an event
  admitted before is delivered, possibly while close runs, and close does not wait for it, so a handler may itself
  call the client or close it. Formatting an event, by any path (in Go every verb, a wrapper field of any
  visibility, JSON and structured logs; reflection cannot reach the params either), shows its kind, its method
  scrubbed and quoted with every unprintable character escaped as Go's `strconv.Quote` does, and the size of its
  params, never the params. Without a handler, notifications are ignored as before. Stderr is kept up to 64 KiB,
  scrubbed, for diagnostics a caller chooses to show; it is never written to a log line,
  because the scrubber cannot recognise every credential format.
  Every call has a caller-supplied timeout, which bounds writing the request as well as waiting for the reply (a
  child that stops reading cannot hold a call); expiry is `codex-error`.
- **A failed start.** When the start fails once the child is running (the handshake below fails, times out or is
  cancelled), the client closes the child and the error carries what it left: the stderr, scrubbed when captured and
  again when read, which no description, formatting path, log line or reflection shows; and, for a child that
  exited by itself, its exit status, written `exit status N` or `signal N` and named in the description. A child
  whose output ended is given 0.5 s to exit by itself first; a child the client had to stop, or one the kernel
  reaped itself, names no exit status: the signal would be the client's own, or the status is gone. The credentials
  error keeps both, through its cause in Go and as its own `stderr` in Swift, the backoff's refusals included.
- **Sequence.**
  1. `initialize` with `clientInfo` and `capabilities`, which is empty unless the caller asks for the experimental
     API (`{"experimentalApi": true}`); the version is the number after the first `/` in `userAgent`. A version below
     `0.161.0` is `codex-too-old`. No parsable version proceeds. The minimum is the oldest release measured: on
     2026-10-16, 0.161.0 answered every method this contract calls, with the same reply shapes as 0.162.1.
  2. `initialized` (a notification).
  3. `getAuthStatus {includeToken: false, refreshToken: false}`. `requiresOpenaiAuth: false` is
     `provider-without-openai-auth`. `authMethod: null` (or absent) is `not-signed-in`; `apikey` is `api-key`; any
     other string but `chatgpt`, `""` included, is `unsupported-auth`.
  4. Only for `chatgpt`: `getAuthStatus {includeToken: true, refreshToken: false}`. `requiresOpenaiAuth` and the
     method are checked again on this reply, and on every refresh reply; a change is classified as in step 3. A
     missing or empty `authToken` is `codex-error`.
- **`-32601` on `getAuthStatus`** (Codex removed the deprecated method). Close the child, start a fresh one, and call
  `config/read {includeLayers: false}`; a running app-server keeps the store it started with, so the answer must
  come from a new child. Its `result.config.cli_auth_credentials_store` decides: `file` (or absent) uses the file
  source below; `keyring`, `auto` and `ephemeral` are `app-server-auth-unavailable`, so a stale `auth.json` left after
  a move to the keychain is never used. A Codex that lacks `config/read` too cannot confirm the file:
  `app-server-auth-unavailable`; any other `config/read` failure is `codex-error`. A `-32601` on the token call
  (step 4) or on a refresh follows the same rule; a token read from the file after a refresh's `-32601` is not
  refreshed again.
- **Read-only `CODEX_HOME`.** Codex cannot start there (it creates state before `initialize`); that exit is
  `codex-error`, not a reason to read the file.

## The file source (`AUTH-02`)

- **Location.** `$CODEX_HOME/auth.json` when `CODEX_HOME` resolves, after following links, to a folder the user
  owns, and, when the caller confines it (a sandboxed app confines it to the home folder; the fixtures run confined
  to `$home`), inside that folder; otherwise a set `CODEX_HOME` is `codex-error`. Unset: `~/.codex/auth.json`. A
  set `CODEX_HOME` is opened once, through the confining folder when there is one, and every read, the re-read
  included, goes through that open folder, whose ownership is checked on its descriptor: replacing a folder of the
  path afterwards cannot redirect a read. The resolved path is opened one folder at a time, each relative to the one
  before it, never through a link and never blocking, so a folder replaced with a link between resolution and opening,
  or a FIFO named as a folder, is `codex-error`. It is inside the confining folder when one of the folders opened is
  that folder, compared by device and inode, never by path: a path spelt in another case on a case-insensitive
  volume, or reaching the folder through another mount point (macOS's `/System/Volumes/Data`), names the same folder.
- **Read.** One `open` with `O_NOFOLLOW | O_NONBLOCK`; `fstat` on that descriptor must show a regular file owned by
  the user and smaller than 64 KiB (65,536 bytes is refused); the bytes are read from that descriptor, bounded. The
  content is JSON in UTF-8 without a byte-order mark (UTF-16 and UTF-32 are not JSON here, as for Codex), with exact
  keys: keys and `auth_mode` values compare as bytes, so a look-alike spelling (a Kelvin sign for `K`) is not the
  name it resembles.
  A link, a FIFO, a folder, a foreign owner or an oversize file is `codex-error`. A missing file is
  `no-sign-in-file`. When the file source was only the fallback for a missing executable, a missing or unusable
  file (and a failing `CODEX_HOME`) is `no-codex` instead: with no usable file either, the fix is to install Codex. The raw
  bytes are wiped after decoding. The file is never written.
- **Parse.** The file is strict JSON as replies are (one value, valid UTF-8, no duplicate key, nested at most 64
  levels, the top-level object included); anything else is a parse failure.
- **Torn write.** A parse failure is retried once after a short delay (Codex may be rewriting the file); a second
  failure is `codex-error`.
- **Mode**, in Codex's own order (`AuthDotJson::resolved_mode`): `auth_mode` when present (an unknown value makes the
  file unreadable to Codex: `codex-error`); else `personal_access_token`, `bedrock_api_key`, `bedrock_access_keys`
  when present and non-null (`unsupported-auth`); else `OPENAI_API_KEY` when present and non-null, even empty
  (`api-key`); else `chatgpt`. `chatgpt` needs a non-empty `tokens.access_token`, else `codex-error`.
- The file source cannot refresh.

## Expiry and refresh (`AUTH-04`)

- **Expiry** is the JWT `exp` claim (exactly `exp`), read without checking the signature. A token that is not a JWT
  (its payload outside the unpadded base64url alphabet, `=` padding and a CR or LF included, or not strict JSON), or whose `exp` is absent or
  not a signed 64-bit integer (a fraction, an overflow), has unknown expiry: it is `ready`, and the provider decides
  (a refusal leads to a refresh, below). Every signed 64-bit `exp` is an instant: the first second of year 1 is
  expired, and the largest is far in the future.
- **Margin.** A token whose `exp - now` is below the caller's margin (recite: 10 min) is refreshed before use.
- **Refresh** goes through `getAuthStatus {includeToken: true, refreshToken: true}` only, never through the file.
  It is single-flight. A refresh **succeeds when the token it returns is fresh** beyond the margin, changed or not;
  a still-stale token is a failure.
- **After a refusal.** A request refused as `auth-expired` asks for a refresh with the refused token. If another
  caller already replaced it with a token that is still fresh, or a refresh completed while this caller waited (even
  one that returned the same bearer), that token is returned without a second refresh. Until a refresh succeeds, or
  the file holds another token, the refused token is not handed out again, though its expiry looks fresh.
- **Failure** fails the current call as `expired`, unless the refresh reply named another sign-in, which is that
  sign-in's state (`not-signed-in`, `api-key`, `unsupported-auth`, `provider-without-openai-auth`, as in sequence
  step 4). Either way it spends no permanent budget: the next refresh is allowed after a
  backoff of 60 s, doubling to at most 15 min, reset by a success, counted from when the failure completed. Within
  the backoff a call fails as `expired` without starting Codex. The failed refresh and every call the backoff refuses
  carry the time the next refresh may run (Go `Error.RetryAt`, Swift `CodexAuthError.retryAt`), and a refusal's cause
  is the failure that started the wait. A refresh the caller cancelled is not a failure and spends no backoff.

## Hygiene (`AUTH-05`)

Every token and `ek_` secret is registered with the scrubber before use. The scrubber replaces registered secrets
and anything shaped like a JWT (`eyJ…​.…​.…`) or an `ek_` secret with `<redacted>`, the union of every match found in
the original text, overlapping or touching matches as one. It keeps no registered secret: each is held as its UTF-8
length and two polynomial hashes modulo 2^61−1 with bases drawn at random per process, and found by rolling both
over the text, so a dropped token leaves nothing of itself in the registry. Any other text matches with probability
below (L/2^61)² per window of length L. Every string an
error or a log line carries from Codex, a file or the network passes through it, and an error's text is scrubbed
whole when formatted. A reported mode is filtered to printable characters before it is scrubbed, because removing
a character can join a secret's pieces. Token values and the inherited environment are never part of a type's
printed, reflected or encoded form.

The debug log (Go `Config.Logger`, Swift `log`) names each decision: a fetch (`codexauth: fetched a token`, or
`codexauth: fetch failed` with the state and class), a refresh (`codexauth: refreshed the token`, `codexauth: refresh
failed` with the state, the backoff and the retry time, `codexauth: refresh cancelled; no backoff spent`) and a call
the backoff holds back (`codexauth: refresh held back by the backoff`). A line carries the source, state, class and
times only, never a token or the child's output.
