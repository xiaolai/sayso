# Fixtures

Every implementation runs every fixture of every component it declares (`capabilities.json`, [parity](parity.md)).
Fixtures are data: a case says what to set up and what to observe, never how an implementation does it.

## Files

| Path | Component | One case is |
|---|---|---|
| `said-guard.json` | `said-guard` | a script, a transcript, a language and the expected verdict |
| `auth-file/*.json` | `credentials` | an `auth.json` (or its absence, or a hostile file), a `CODEX_HOME`, and the expected state |
| `app-server/*.jsonl` | `credentials` | a scripted Codex app-server, a setup, and the expected state, requests and child environment |
| `mint/*.json` | `mint` | an origin, scripted HTTP replies, and the expected secret or error class |
| `ws/*.json` | `session` | a WebSocket close code, an `error` event, or a message size, and the expected class |
| `events/*.json` | `session` | a raw server event and the expected typed result or protocol error |
| `pcm16.json` | `pcm16` | an encode, decode, WAV or plausibility vector |

A `.json` file holds an array of cases; a `.jsonl` file holds one case per line. Every case has a unique `id`
across all files and names its `component`. A case may list `requires` (capabilities); see [parity](parity.md).
`note` explains a case and is never read by a harness.

## Conventions

- **Clock.** Credential and mint cases run at `setup.now` (Unix seconds, default `1800000000`); implementations take
  an injectable clock.
- **JWTs.** Anywhere in a case, the object `{"$jwt": {...claims}}` stands for an unsigned JWT built as
  `base64url('{"alg":"none","typ":"JWT"}') + "." + base64url(claims as compact JSON, keys sorted) + "." +
  base64url("sig")`, with no padding. `{"$jwt": {}}` is a JWT without `exp`. Harnesses and the fake app-server
  substitute it before use, so a fixture states the expiry instead of hiding it in an opaque string.
- **Paths.** In expectations, `$home`, `$codexHome` and `$binDir` are the harness's temporary home folder, the
  `CODEX_HOME` in force, and the folder holding the `codex` executable.
- **Secrets.** Fixture tokens are fake. A value starting `canary` must never appear in any error description or log
  line an implementation produces (`expect.notInDescription`).

## The fake app-server

`tools/fake-app-server.py` is the one scripted `codex` both test suites run. A harness installs it as `$binDir/codex`
and passes the case's `setup.server` object, written to a file, in `SAYSO_FAKE_SCENARIO`. Each start appends one
JSON line `{"spawn": {"argv", "env", "pid"}}` to `SAYSO_FAKE_RECORD`, and each received message
`{"req": <message>, "pid"}`. Scripted replies count calls across every start that shares the record, because a
client starts one child per fetch or refresh. The scenario keys are documented at the top of the script.

## Case shapes

### `said-guard`

`script`, `transcript`, `language` (BCP 47), `expect` (bool), optional `requires`.

### `credentials` (`auth-file/`, `app-server/`)

`setup`:

| Key | Meaning |
|---|---|
| `now`, `marginSeconds` | the clock and the refresh margin |
| `source` | `file` (the file source, chosen), `fallback` (the file source as the fallback for a missing executable), `auto` (resolve Codex, then fall back) |
| `codex` | `fake` (the fake at `$binDir/codex`, `$binDir` on `PATH`); `npm-shim` (`$binDir/codex` is `#!/usr/bin/env sayso-fake-node`, with `$binDir/sayso-fake-node` running the fake); `none` (nothing resolvable); `explicit-missing` (the caller names `$home/missing/codex`); `candidate:<dir>` (the fake at `$home/<dir>/codex`, off `PATH`); `nvm:<v>,<v>,…` (a fake in each `$home/.nvm/versions/node/<v>/bin`) |
| `inheritedPath` | the `PATH` the caller inherits (default: `$binDir:/usr/bin:/bin`) |
| `inheritedEnv` | extra inherited variables |
| `codexHome` | `default` (unset; `$home/.codex`), `inside` (`$home/custom-codex`), `outside` (a folder outside `$home`), `symlink-outside` (`$home/link` → outside), `file-not-folder` (`$home/codex-file`, a file) |
| `file` | `auth.json` in the `CODEX_HOME` in force: `{"kind": "regular", "json": …}`, `{"kind": "regular", "raw": "…"}`, `{"kind": "regular", "rawRepeat": {"text", "count"}}`, `{"kind": "regular", "hex": "…"}` (bytes), `{"kind": "missing"}`, `{"kind": "symlink", "json": …}` (a link to a file elsewhere in `$home`), `{"kind": "fifo"}`, `{"kind": "directory"}`; `padTo` pads `json` with trailing spaces to that many bytes |
| `retry` | the content the file holds at the one re-read after a parse failure |
| `timeoutMs` | the per-call app-server timeout |
| `server` | the fake's scenario (top of `tools/fake-app-server.py`) |

`expect` (or, for a timeline, each step's `expect`):

| Key | Meaning |
|---|---|
| `state` | the state ([credentials](credentials.md)) |
| `token`, `expiresAt` | for `ready`: the token and its expiry (`null`: unknown) |
| `mode`, `codexVersion` | the reported mode or version |
| `spawns` | how many times `codex` was started |
| `argv` | the arguments of every start |
| `requests` | the exact sequence of requests and notifications the fake received, across starts (the client's answers to server requests are not counted): `method`, `params` that must match exactly when given, `notification: true` for a message without `id` |
| `envAbsent`, `envEquals`, `pathStartsWith` | the child's environment |
| `binDir` | the folder the resolved executable is in |
| `childrenExited` | no started child is alive when the probe returns, and no process the fake started in the child's group (`groupDescendant`) is alive a moment later, once its new parent has reaped it |
| `closeWithinMs` | the probe, including closing the child, returns within this many milliseconds |
| `serverRequestAnswered` | `error`: the server request got an error reply |
| `refreshCalls` | `getAuthStatus` calls with `refreshToken: true` so far |
| `notInDescription` | strings that no error description or log line may contain |

A client case has `setup.client` and drives the app-server client itself, started with the fake at
`$binDir/codex`, the case's environment and timeout: `handler` (`record`: keep every event; `block`: never return
until the case ends; absent: no handler), `dir` (the working directory: `work`, a new `$work` folder in the case's
temporary folder; `relative`; `missing`; `file`; absent: none), `experimentalAPI`, and `calls` (`method`, `params`) made
in order. Its `expect` adds `startFails`, `calls` (`ok` or `error` per call), `callError` (text every failed call's
error contains), `events` (exactly these, in order:
`method`; `params`, compared as JSON, with `$work` and `$callerCwd`, the harness's own working directory, after links
are followed, replaced; `noParams: true` for none; `serverRequest: true`), `eventCount`, and `eventSequence` (the params
member that holds each event's index); the other keys above apply too. The harness waits up to 5 s for the expected
number of events, then 100 ms for any that should not come, before it closes the client.

A timeline case has `steps` instead of `expect`: each runs `op` (`token`, or `refreshAfter` with `stale`: the
`initial` or the `current` token) at `now + at` seconds on one credentials object.

### `mint` (`mint/`)

`setup`: `origin` (`test`: the harness's fake server, registered as a test origin; or a URL that must be refused),
`session`, `lifetimeSeconds`, `token` (what the stub credential source returns), `refreshTo` (what it returns after a
refresh), `replies` (in order: `status`, `headers`, and `json` or `body`; `bodyPadTo` pads with trailing spaces).
`expect`: `class` (`null`: success), `secret`, `expiresAt`, `status`, `type`, `code`, `param`, `requestId`, `requests`
(requests the fake received), `refreshCalls`, `request` (the first request: method, path, headers, JSON body),
`secondAuthorization`, `scrubs` (strings the scrubber must replace afterwards), `notInDescription`.

### `session` (`ws/`, `events/`)

`kind`: `close` (`code`, `reason`) → `expect.class`; `error-event` (`event`) → `expect.class`, `fatal`, `code`,
`type`, `param`; `size` (`readLimit`, `messageBytes`) → `expect.class` (`null`: accepted); `event` (`raw`, the exact
text) → `expect` with `type` and fields, or `protocolError` (the type the error must name; `null`: it need not name one),
`ignored`, or `unknown` (the type). `pcmHex` is the decoded audio.

### `pcm16` (`pcm16.json`)

`op`: `encode` (`input`, with `"NaN"`, `"Infinity"`, `"-Infinity"` as strings) → `expectSamples`, `expectHex`;
`decode` (`hex`, `sliceFrom`) → `expectSamples` (each float is the sample / 32768); `wav` (`rate`, `pcmHex`) →
`expectHex`, or `expectError: true`; `validWAV` (`hex`, `capBytes`) → `expect`; `plausible` (`pcm`: `samples` copies of `value`, `script`)
→ `expect`.
