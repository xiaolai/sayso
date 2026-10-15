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
| `codex-too-old` | Codex reports a version below the minimum, `0.162.0` |
| `not-signed-in` | Codex reports no sign-in (`authMethod: null`) |
| `no-sign-in-file` | the file source was chosen and `auth.json` does not exist; the sign-in may be in the keychain |
| `api-key` | Codex is signed in with an API key |
| `unsupported-auth` | any other sign-in mode; the mode is reported, at most 64 printable characters, scrubbed |
| `expired` | the token is within the caller's margin of expiry and a refresh did not renew it |
| `codex-error` | Codex failed: did not start, crashed, timed out, answered malformed or with an error, or the file is unusable |
| `app-server-auth-unavailable` | Codex lacks `getAuthStatus` and its effective store is not a file |
| `provider-without-openai-auth` | Codex reports `requiresOpenaiAuth: false`: a provider needing no OpenAI sign-in is in force |

Apps map states to their own text; this package has no user-facing strings.

## Executable (`AUTH-01`)

In order: an explicit path from the caller; then `PATH`; then the GUI candidates, because an app launched from the
Finder has no login-shell `PATH`: `/opt/homebrew/bin`, `/usr/local/bin`, then `~/.local/bin`, `~/.npm-global/bin`,
`~/.volta/bin`, `~/.bun/bin`, `~/Library/pnpm`, then every `~/.nvm/versions/node/*/bin`, newest version first by
numeric comparison. The result is absolute, a file, and executable. An explicit path that does not resolve is
`no-codex`, never a fall-through to the list.

## The app-server source (`AUTH-01`, `AUTH-03`)

- **Child.** `<codex> -c model_provider="openai" app-server --listen stdio://`. The provider override makes Codex
  answer for its OpenAI sign-in even when the user's configured provider needs none.
- **Environment.** Inherited, minus these names compared case-insensitively: `OPENAI_API_KEY`, `CODEX_API_KEY`,
  `CODEX_ACCESS_TOKEN`, `CODEX_APP_SERVER_LOGIN_CLIENT_ID`, `CODEX_REFRESH_TOKEN_URL_OVERRIDE`,
  `CODEX_REVOKE_TOKEN_URL_OVERRIDE`. `CODEX_HOME` is set when the caller configured one. `PATH` is the
  executable's folder, then `/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin`, then the inherited `PATH`, so an
  npm-installed `codex` (`#!/usr/bin/env node`) finds its interpreter. Nothing else is added.
- **Lifetime.** Started on demand for one fetch or refresh, then closed; the token is cached in memory, not the
  child. The child starts in its own process group. Close: stdin closed, `SIGTERM` to the group, a 2 s grace,
  `SIGKILL` to the group; close returns within grace + 1 s even when a descendant outside the group still holds the
  pipes (our ends are closed and the readers abandoned).
- **Messages.** Newline-delimited JSON-RPC. A line longer than 4 MiB, or a line that is not JSON, is `codex-error`.
  Notifications are ignored. A server request (a message with both `id` and `method`, such as an approval) is
  answered with a JSON-RPC error and never acted on. Stderr is kept up to 64 KiB, scrubbed, for diagnostics only.
  Every call has a caller-supplied timeout; expiry is `codex-error`.
- **Sequence.**
  1. `initialize` with `clientInfo`; the version is the number after the first `/` in `userAgent`. A version below
     `0.162.0` is `codex-too-old`. No parsable version proceeds.
  2. `initialized` (a notification).
  3. `getAuthStatus {includeToken: false, refreshToken: false}`. `requiresOpenaiAuth: false` is
     `provider-without-openai-auth`. `authMethod: null` is `not-signed-in`; `apikey` is `api-key`; anything but
     `chatgpt` is `unsupported-auth`.
  4. Only for `chatgpt`: `getAuthStatus {includeToken: true, refreshToken: false}`. The method is checked again on
     this reply; a changed method is classified as in step 3. A missing or empty `authToken` is `codex-error`.
- **`-32601` on `getAuthStatus`** (Codex removed the deprecated method). Close the child, start a fresh one, and call
  `config/read {includeLayers: false}`; a running app-server keeps the store it started with, so the answer must
  come from a new child. Its `result.config.cli_auth_credentials_store` decides: `file` (or absent) uses the file
  source below; `keyring`, `auto` and `ephemeral` are `app-server-auth-unavailable`, so a stale `auth.json` left after
  a move to the keychain is never used.
- **Read-only `CODEX_HOME`.** Codex cannot start there (it creates state before `initialize`); that exit is
  `codex-error`, not a reason to read the file.

## The file source (`AUTH-02`)

- **Location.** `$CODEX_HOME/auth.json` when `CODEX_HOME` resolves, after following links, to a folder the user owns
  inside the home folder; otherwise a set `CODEX_HOME` is `codex-error`. Unset: `~/.codex/auth.json`.
- **Read.** One `open` with `O_NOFOLLOW | O_NONBLOCK`; `fstat` on that descriptor must show a regular file owned by
  the user and smaller than 64 KiB (65,536 bytes is refused); the bytes are read from that descriptor, bounded.
  A link, a FIFO, a folder, a foreign owner or an oversize file is `codex-error`. A missing file is
  `no-sign-in-file` (or `no-codex` when the file source was only the fallback for a missing executable). The raw
  bytes are wiped after decoding. The file is never written.
- **Torn write.** A parse failure is retried once after a short delay (Codex may be rewriting the file); a second
  failure is `codex-error`.
- **Mode**, in Codex's own order (`AuthDotJson::resolved_mode`): `auth_mode` when present (an unknown value makes the
  file unreadable to Codex: `codex-error`); else `personal_access_token`, `bedrock_api_key`, `bedrock_access_keys`
  when present and non-null (`unsupported-auth`); else `OPENAI_API_KEY` when present and non-null, even empty
  (`api-key`); else `chatgpt`. `chatgpt` needs a non-empty `tokens.access_token`, else `codex-error`.
- The file source cannot refresh.

## Expiry and refresh (`AUTH-04`)

- **Expiry** is the JWT `exp` claim, read without checking the signature. A token that is not a JWT, or has no
  `exp`, has unknown expiry: it is `ready`, and the provider decides (a refusal leads to a refresh, below).
- **Margin.** A token whose `exp - now` is below the caller's margin (recite: 10 min) is refreshed before use.
- **Refresh** goes through `getAuthStatus {includeToken: true, refreshToken: true}` only, never through the file.
  It is single-flight. A refresh **succeeds when the token it returns is fresh** beyond the margin, changed or not;
  a still-stale token is a failure.
- **After a refusal.** A request refused as `auth-expired` asks for a refresh with the refused token; if another
  caller already replaced it, the replacement is returned without a second refresh.
- **Failure** fails the current call as `expired` and spends no permanent budget: the next refresh is allowed after a
  backoff of 60 s, doubling to at most 15 min, reset by a success. Within the backoff a call fails as `expired`
  without starting Codex.

## Hygiene (`AUTH-05`)

Every token and `ek_` secret is registered with the scrubber before use. The scrubber replaces registered secrets
(longest first) and anything shaped like a JWT (`eyJ…​.…​.…`) or an `ek_` secret with `<redacted>`. Every string an
error or a log line carries from Codex, a file or the network passes through it. Token values are never part of a
type's printed or encoded form.
