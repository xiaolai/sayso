# sayso

Shared building blocks for apps that use a local Codex sign-in to drive OpenAI's Realtime API. Specified once, and
implemented in Go and Swift against one shared set of fixtures.

Status: early. The contract and its fixtures cover every component; only the said guard is implemented so far, in Go.

| Component | Spec | Go | Swift |
|---|---|---|---|
| Credentials: the local Codex sign-in | [`spec/credentials.md`](spec/credentials.md) | not yet | not yet |
| Minting a Realtime client secret | [`spec/http-and-mint.md`](spec/http-and-mint.md) | not yet | not yet |
| WebSocket session and typed events | [`spec/session.md`](spec/session.md) | not yet | not yet |
| PCM16 audio | [`spec/pcm16.md`](spec/pcm16.md) | not yet | not yet |
| Said guard: did the model read exactly this script? | [`spec/said-guard.md`](spec/said-guard.md) | `go/saidguard` | not yet |

How the implementations are held to one behaviour: [`spec/parity.md`](spec/parity.md). Fixture format:
[`spec/fixtures.md`](spec/fixtures.md).

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

## Test

```sh
python3 tools/manifest.py --check
python3 -m unittest discover -s tools -p 'test_*.py'
(cd go && go vet ./... && go test ./...)
swift test
```

The Go tests read `../fixtures` from this checkout, and fail if the fixtures are missing.

## Licence

ISC. `go/internal/numwords` comes from [recite](https://github.com/xiaolai/recite).
