# sayso

Shared building blocks for apps that use a local Codex sign-in to drive OpenAI's Realtime API, and for apps that ask an
OpenAI-compatible service, a local model or a reader's own CLI a question. Specified once, and implemented in Go and
Swift against one shared set of fixtures where the contract is the wire.

Status: early. The contract and its fixtures cover the five components both implementations share. Of the AI
providers, the OpenAI-compatible client is Swift-only until its fixtures and Go client land (see
[Parity](spec/chat-completions.md#parity)), and its opt-in streamed delivery has been tested against a stand-in server
only, not a live one; the CLI and Apple on-device providers are Swift-only by design.

| Component | Spec | Go | Swift |
|---|---|---|---|
| Credentials: the local Codex sign-in, and a private Codex home's | [`spec/credentials.md`](spec/credentials.md) | `go/codexauth`, `go/codexapp` | `CodexAuth` (macOS) |
| Minting a Realtime client secret | [`spec/http-and-mint.md`](spec/http-and-mint.md) | `go/realtime`, `go/openaihttp` | `RealtimeConnection` |
| WebSocket session and typed events | [`spec/session.md`](spec/session.md) | `go/realtime` | `RealtimeConnection` |
| PCM16 audio | [`spec/pcm16.md`](spec/pcm16.md) | `go/pcm16` | `PCM16` |
| Said guard: did the model read exactly this script? | [`spec/said-guard.md`](spec/said-guard.md) | `go/saidguard` | `SaidGuard`, with ICU spell-out for `ar de es fr hi it ja ko pt` |
| AI providers: one OpenAI-compatible client, provider profiles, model lists | [`spec/chat-completions.md`](spec/chat-completions.md) | pending (`go/aiproviders`) | `SaySoAI` |
| The reader's Claude and Codex CLIs as providers | [`spec/cli-providers.md`](spec/cli-providers.md) | — (process-bound, Swift-only) | `SaySoAICLI` (macOS) |
| Apple's on-device model, guided into the caller's answer type | [`spec/apple-on-device.md`](spec/apple-on-device.md) | — (framework-bound, Swift-only) | `SaySoAIApple` (macOS 26.4 / iOS 26.4) |

Both implement the scrubber and the redacting secret types: Go in `go/secret`, Swift in `RealtimeConnection`. Go
alone has `go/numwords` (Swift's number words are internal to `SaidGuard`), `codexapp.Login`, which runs
`codex login`, and `codexapp.PlanStatus`, which reads the plan's rate limits; Swift alone has `RealtimeDeadline`.
These are helpers outside the contract, which no fixture checks directly. Go keeps the failure classes and the
classified error in the leaf package `go/failure`, which `go/openaihttp` names again, so `go/codexapp` and
`go/codexauth` build without the HTTP stack. `CodexAuth` builds on macOS only, and so does `SaySoAICLI`;
`SaySoAIApple` starts at macOS 26.4 / iOS 26.4; the other products also build for iOS 17. `SaySoAI`, `SaySoAICLI`
and `SaySoAIApple` are written in Swift 6's language mode — the concurrency checking these protocol types need; the
older products keep the mode they shipped with.

How the implementations are held to one behaviour: [`spec/parity.md`](spec/parity.md). Fixture format:
[`spec/fixtures.md`](spec/fixtures.md).

## Example applications

| App | What it is | Uses |
|---|---|---|
| [recite](https://github.com/xiaolai/recite) | A text-to-speech CLI that checks every recording was read exactly as written | The Go module, since recite 0.3.0 |
| [Mochi](https://github.com/xiaolai/mochi-macOS) | A native macOS companion for English conversation and voice practice | The Swift package (migration not yet released) |
| [Checkmate](https://github.com/xiaolai/checkmate) | Native macOS chess practice with offline Stockfish and a spoken coach | The Swift package (migration not yet released) |

## The route is undocumented

Using a ChatGPT sign-in, through Codex, to mint Realtime client secrets is not a documented OpenAI route, and the
Codex method that returns the token (`getAuthStatus`) is deprecated upstream. OpenAI's Sign in with ChatGPT preview
limitations, which apply to ChatGPT plan usage through `https://api.openai.com/v1` directly and through Codex
app-server, state that "Audio/video input, the Files upload API, and the transcription API are not supported by this
flow". That page describes the Responses flow; whether it covers Realtime sessions is an inference, not established.
What is established is that OpenAI documents no audio support for ChatGPT plan usage. The route works today and may
be refused at any time, which is why every failure here is a named state or error class, never a crash.

Use it only with your own sign-in, on your own devices: OpenAI's Terms of Use forbid sharing account access.

## Layout

- `spec/`: the contract each implementation must meet.
- `fixtures/`: test cases shared by every implementation. `MANIFEST.json` lists them; regenerate it with
  `tools/manifest.py`, and check it with `tools/manifest.py --check`.
- `go/`: the Go module `github.com/xiaolai/sayso/go`, tagged `go/vX.Y.Z`.
- `Package.swift`, `Sources/`, `Tests/`: the Swift package `SaySo`, tagged `vX.Y.Z`.
- `tools/fake-app-server.py`: the scripted `codex` both test suites run.
- `tools/differential.py`: runs Go and Swift on the same generated inputs and fails on any disagreement.

## Test

```sh
python3 tools/manifest.py --check
python3 -m unittest discover -s tools -p 'test_*.py'
(cd go && go vet ./... && go test ./...)
swift test
python3 tools/differential.py
```

The Go tests read `../fixtures` from this checkout, and fail if the fixtures are missing. `differential.py` needs both
toolchains; `--seed` and `--count` choose its cases (spec/parity.md).

## Licence

ISC. `go/numwords` and much of the Go module come from [recite](https://github.com/xiaolai/recite).
