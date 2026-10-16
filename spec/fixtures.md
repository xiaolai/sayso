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
`note` explains a case and is never read by a harness. A member this document does not define for its component is
refused, by `tools/manifest.py` and by every harness: no harness would read it, so the case would pass whatever it
says.

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

`script` and `transcript` are each a string or a repeat, `{"repeat": r, "times": n, "prefix": p, "suffix": s}`, which
stands for `p`, then `r` written `n` times, then `s`, so a long input is stated rather than pasted. A repeat has exactly
these four members: `r` a non-empty string, `p` and `s` strings (`""` for none), `n` a whole number of at least 1, and
the text it stands for is at most 1 MiB (1,048,576 bytes of UTF-8). Harnesses expand a repeat as they load the case and
fail on any other shape.

A case whose id starts `bound-` is an input that once crashed the guard or took seconds under the size cap. Besides its
verdict, each test suite bounds the CPU time it takes, loosely enough to tell a linear algorithm from a quadratic one,
with more room for an instrumented or debug build.

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
| `cancelAfterMs` | the caller cancels the probe this many milliseconds after it starts |
| `server` | the fake's scenario (top of `tools/fake-app-server.py`) |

`expect` (or, for a timeline, each step's `expect`):

| Key | Meaning |
|---|---|
| `state` | the state ([credentials](credentials.md)) |
| `class` | the error's class ([HTTP and mint](http-and-mint.md)); `""` for no error |
| `token`, `expiresAt` | for `ready`: the token and its expiry (`null`: unknown) |
| `mode`, `codexVersion` | the reported mode or version |
| `spawns` | how many times `codex` was started |
| `argv` | the arguments of every start |
| `requests` | the exact sequence of requests and notifications the fake received, across starts (the client's answers to server requests are not counted): `method`, `params` that must match exactly when given, `notification: true` for a message without `id` |
| `envAbsent`, `envEquals`, `pathStartsWith` | the child's environment |
| `binDir` | the folder the resolved executable is in |
| `childrenExited` | no started child is alive when the probe returns, and no process the fake started in the child's group (`groupDescendant`) is alive a moment later, once its new parent has reaped it |
| `closeWithinMs` | the probe, including closing the child, returns within this many milliseconds |
| `serverRequestAnswered` | `error`: the server request got an error reply with code `-32601` |
| `outbound` | every message the fake read, in order: its `method`, or `refusal:<id>` for an answer to a server request |
| `refreshCalls` | `getAuthStatus` calls with `refreshToken: true` so far |
| `notInDescription` | strings that no error description or log line may contain |
| `inDescription` | strings the error description must contain |
| `inLog` | strings some debug line so far must contain |
| `retryAt` | when the error says the next refresh may run, in Unix seconds (`null`: it names no time) |
| `stderr` | what the child wrote to stderr, as the error carries it from a failed start (`""`: none) |

A client case has `setup.client` and drives the app-server client itself, started with the fake at
`$binDir/codex`, the case's environment and timeout: `handler` (`record`: keep every event; `block`: never return
until the case ends; absent: no handler), `dir` (the working directory: `work`, a new `$work` folder in the case's
temporary folder; `relative`; `missing`; `file`; absent: none), `experimentalAPI`, and `calls` (`method`, `params`) made
in order; a call with `typed: true` goes through the client's typed wrapper for its method instead (`account/read`:
the plan type). Its `expect` adds `startFails`, `calls` (`ok` or `error` per call), `callError` (text every failed call's
error contains), `typedResults` (each typed call's result, in order; `null` for one that failed), `events` (exactly these, in order:
`method`; `params`, compared as JSON, with `$work` and `$callerCwd`, the harness's own working directory, after links
are followed, replaced; `paramsRaw`, the exact bytes; `noParams: true` for none; `serverRequest: true`), `eventCount`,
and `eventSequence` (the params
member that holds each event's index); the other keys above apply too. The harness waits up to 5 s for the expected
number of events, then 100 ms for any that should not come, before it closes the client.

A timeline case has `steps` instead of `expect`: each runs `op` (`token`, or `refreshAfter` with `stale`: the
`initial` or the `current` token) at `now + at` seconds on one credentials object.

### `mint` (`mint/`)

`setup`: `origin` (`test`: the harness's fake server, registered as a test origin; or a URL that must be refused),
`session`, `lifetimeSeconds`, `token` (what the stub credential source returns), `refreshTo` (what it returns after a
refresh), `replies` (in order: `status`, `headers`, and `json` or `body`; `bodyPadTo` pads with trailing spaces).
`expect`: `class` (`null`: success), `secret`, `expiresAt`, `status`, `type`, `code`, `param`, `requestId`, `requests`
(requests the fake received), `refreshCalls`, `tokenCalls` (tokens asked of the stub), `request` (the first request:
method, path, headers, JSON body), `secondAuthorization`, `scrubs` (strings the scrubber must replace afterwards),
`notInDescription`.

### `session` (`ws/`, `events/`)

`kind`: `close` (`code`, `reason`) → `expect.class`; `error-event` (`event`) → `expect.class`, `fatal`, `code`,
`type`, `param`; `size` (`readLimit`, `messageBytes`) → `expect.class` (`null`: accepted); `event` (`raw`, the exact
text) → `expect` with `type` and fields, or `protocolError` (the type the error must name; `null`: it need not name one),
`ignored`, or `unknown` (the type). `pcmHex` is the decoded audio. `handshake` dials a fake server that reads the
upgrade request and answers it with `reply` (`status`, `headers`, `body`; `hold` keeps the connection open after it),
or, with `silent: true`, never answers; `handshakeTimeoutMs` defaults to 5000. Both harnesses write the reply as
`HTTP/1.1 <status> Fixture`, the headers in name order, `Content-Length` when there is a body, and
`Connection: close` unless it is held. → `expect.class`, `status`, `requestId`, `code`, `withinMs` (the most the
dial may take), `request` (`target`, the request line's target, and `authorization`), `notInDescription`.

### `pcm16` (`pcm16.json`)

`op`: `encode` (`input`, with `"NaN"`, `"Infinity"`, `"-Infinity"` as strings) → `expectSamples` (required),
`expectHex`; `decode` (`hex`, `sliceFrom`) → `expectSamples` (required; each float is the sample / 32768); `wav` (`rate`, `pcmHex`) →
`expectHex`, or `expectError: true`; `validWAV` (`hex`, `capBytes`) → `expect`; `plausible` (`pcm`: `samples` copies of `value`, `script`)
→ `expect`.
