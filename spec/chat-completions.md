# Chat completions over an OpenAI-compatible endpoint

`HTTPProvider` (Swift: `SaySoAI.HTTPProvider`; Go: `go/aiproviders`, pending) answers one composed question against a
reader-chosen endpoint. It is transport, not policy: the caller composes the question and validates the answer; this
contract covers only the wire — where the request goes, what it carries, how a refusal is told apart from a malformed
reply, and how the model list is read.

Sibling providers answering the same `AIProvider` protocol, each with its own contract: the reader's Claude and
Codex CLIs ([`spec/cli-providers.md`](cli-providers.md)) and Apple's on-device model
([`spec/apple-on-device.md`](apple-on-device.md)).

**Threat model.** The endpoint may be a cloud service, a server the reader runs, or something hostile. The key must
travel only to the origin the reader configured; a redirect elsewhere is refused before the header is replayed. The
response body is untrusted: bounded while reading, parsed strictly, and a refusal's text bounded and scrubbed of the key
before it is shown.

## The request

`POST <base>/chat/completions`, `Content-Type: application/json`:

| Field | Value |
|---|---|
| `model` | the caller's model id (required by every cloud service; a local server may ignore it) |
| `messages` | one entry: `{"role": "user", "content": <the composed question>}` |
| `temperature` | `0`, or absent when the profile's models refuse one (Anthropic's) |
| `stream` | `false`, or `true` when the configuration asks for streamed delivery ([below](#streamed-delivery)); never `stream_options`, which some servers refuse as an unknown member |
| `max_completion_tokens` **or** `max_tokens` | the token budget; which one goes first is the profile's preference. An answer's is a positive count (1,024 by default) or absent: a budget of "the endpoint's default" sends neither field, so the server's own limit applies, whatever that is. A probe's is always 1 |

A streamed request also carries `Accept: text/event-stream`. Auth goes on the request according to the profile:
`Authorization: Bearer <key>` for most services, a named header (`api-key`) for Azure-style endpoints, or nothing for
a local server. A key is never sent over plain `http` except to a loopback host.

A budget at or below zero cannot be built: the constructor for a count written in code traps, and the one for a count
read from settings returns nothing.

## Token-field negotiation

OpenAI's current models document `max_completion_tokens` and deprecate `max_tokens`; most compatible services document
only `max_tokens`; DeepSeek accepts `max_completion_tokens` and ignores it while honouring `max_tokens` (measured
2026-10-25). So: send the profile's preferred field first, and on **any** 400 or 422 answer **once** with the other
field. The retry is the status's, never a sentence's — a service need not name the field it refused. Only a retry
that succeeds replaces the first reply; one that fails too reports the first reply's refusal. A request with no token
field has nothing to negotiate and is never retried: the same request again would get the same answer, so its 400 or
422 is a server refusal with the server's own words. Which servers accept a request with no token field, and what
limit they then apply, has not been checked against any live server.

## Answers and refusals

A 2xx body is the OpenAI chat-completion envelope:

- `choices` is a non-empty array; the first element has a `message` object with `content`.
- `content` is a string. `""` with `finish_reason: "length"` is a truncated answer, never a result — except for a
  probe, where an empty or length-capped content still proves the endpoint, the model and the key.
- `content: null` is accepted for a probe only (a reasoning model's one-token turn spends the budget on reasoning); an
  answer with no content is no answer.
- `finish_reason: "content_filter"` or a non-empty `message.refusal` is a refusal.
- An answer needs `finish_reason: "stop"` (or `"length"` for a probe); a missing, null or other reason (`tool_calls`)
  is a bad shape.

Every non-2xx is typed, and the status decides:

| Status | Result |
|---|---|
| 401, 403 | unauthorised |
| 404 | model not found |
| 429 | rate limited |
| 3xx | redirect refused |
| any other (400, 402, 422, 5xx) | **server refusal**, carrying the server's own words |

A server refusal's message is the OpenAI-style `error.message` when the body is JSON, otherwise the raw body; whitespace
is collapsed, the text is capped at 300 characters, and any occurrence of the key is replaced before it is stored. The
message is for the reader's eyes; logs carry the status code only.

A 2xx whose body is not the envelope above is a bad shape, not a refusal.

## Limits

Each configuration sets three limits on one request; a caller that sets none gets 60 s, 90 s and 1 MiB.

- **Idle** (60 s by default): the longest wait for the next byte from the server, counted from the moment the request
  starts, so it covers connecting and waiting for the headers as well as the gap between any two bytes of the body.
  Every byte counts, a stream's comment included. A whole reply sends nothing until it is finished, so for one, idle
  is about the longest an answer may take.
- **Ceiling** (90 s by default, or none): the most one HTTP request may take from start to its last byte. The
  token-field retry is a request of its own and starts a fresh one. With no ceiling, idle and the caller's
  cancellation are the only bounds, so a server that keeps sending bytes (a stream of keep-alive comments, say) holds
  the call until the caller cancels it.
- **Reply cap** (1 MiB by default, at least one byte): the largest 2xx body read whole; one byte more is a bad shape.
  A refusal's body is read up to a fixed 64 KiB and cut there, never failed, and a status worded from its code alone
  (401, 403, 404, 429, 3xx) has its body not read at all. A streamed reply is capped differently
  ([below](#streamed-delivery)).

An idle limit or ceiling that passes is a timeout. A caller's cancellation is a cancellation, even when it lands at the
same moment as a limit. Non-positive limits cannot be built: the idle limit, a present ceiling and the reply cap each
trap at construction when not positive.

**The provider enforces these limits itself, on whatever session it runs.** Each request races a watchdog that sleeps
until the earlier of the last byte plus idle and the start plus the ceiling, and cancels the transfer when it wakes
first. A session injected by the caller (a test stand-in, a shared session) keeps its own limits as well, so the
shorter one wins. The session the provider creates sets URLSession's request timeout to idle and its resource timeout
to the ceiling (seven days, URLSession's own default, when there is none), as a backstop whose `timedOut` is also a
timeout. That URLSession's request timeout restarts whenever data arrives is taken from memory of Apple's
documentation, not re-read; the watchdog makes the contract independent of it.

## Streamed delivery

Opt-in per configuration; a whole reply is the default. A streamed request still returns one complete answer or a
typed failure, never a partial answer. Because a reasoning model's stream keeps sending bytes while it works, the idle
limit then measures real silence rather than the whole of its thinking. There is no automatic fallback to a whole
reply: the caller chooses. A probe uses the configured delivery, so a server that refuses `stream: true` fails its
probe with its own words rather than passing it and failing every answer.

The status arrives before the stream, so the token-field negotiation and the typed non-2xx statuses are unchanged. A
2xx whose `Content-Type` media type, compared without parameters, is `application/json` means the server ignored
`stream: true`: its body is read whole under the reply cap, as above. Any other 2xx, one with no type included, is read
as a stream.

**Framing** follows the WHATWG HTML server-sent events parsing rules, applied to bytes rather than to Foundation's line
reader (which drops the blank lines that dispatch an event, and splits on U+2028 and U+0085):

| Input | Handling |
|---|---|
| line ends | CRLF, LF and a lone CR each end a line; a CR whose LF arrives in the next chunk is one line end |
| a UTF-8 byte order mark at the start | skipped |
| a line starting with `:` | a comment: ignored, but its bytes count as activity for the idle limit |
| `data`, `data:` or `data: x` | one leading space removed; the value is appended to the event's data, lines joined with `\n` |
| any other field (`event`, `id`, `retry`, unknown), with or without a colon | ignored |
| a blank line | dispatches the event; an event with empty data is not dispatched |
| the end of the stream | a last line without its line end is read, and an event not yet dispatched is dispatched |
| U+2028, U+2029 or U+0085 inside data | plain data, never a line end |

Dispatching at the end of the stream is more lenient than WHATWG, which drops the event; a JSON object cut short
fails to parse, so this cannot turn a broken stream into an answer.

**Assembling.** Each event's data is `[DONE]` (after trimming whitespace), which ends the stream, or a JSON object;
anything else is a bad shape.

| Event | Effect |
|---|---|
| a top-level `error`, object or string | a server refusal with the reply's HTTP status (normally 200) and the server's words, worded as for a refusal's body: scrubbed of the key and capped at 300 characters |
| no `choices`, or `choices: []` (a usage-only chunk) | ignored |
| `choices` that is not an array of objects | a bad shape |
| a choice whose `index` is not the JSON integer 0 | ignored (no request asks for more than one); a missing `index` counts as 0, but one that is a string (`"0"`, `"1"`), a boolean, a fraction or null is another choice |
| `delta.content`, a string | appended to the answer |
| `delta.content` null or missing | nothing |
| `delta.content` of any other type | a bad shape |
| `delta.refusal`, a non-empty string | marks the turn refused; its text is not kept |
| any other `delta` key (`reasoning_content`, `reasoning`, `reasoning_details`, `role`, `tool_calls`, ...) | ignored: only `content` builds the answer, an allowlist rather than a list of reasoning fields to drop |
| the first non-null `finish_reason` for choice 0 | ends the answer; the rest of the transfer is cancelled, not read. A reason that is not a string is an unknown one |

`<think>…</think>` written inside `content` is kept, as in a whole reply; the caller validates. Streamed content is not
scrubbed, as a whole reply's is not.

| How the stream ends | Answer | Probe |
|---|---|---|
| `stop`, content non-empty, not refused | the content | ready |
| `stop`, content empty | a bad shape | ready |
| `length` | truncated | ready |
| `content_filter` | refused | refused |
| `stop` on a refused turn (or `length`, for a probe) | refused | refused |
| `tool_calls`, `function_call`, `error`, or any other reason | a bad shape | a bad shape |
| `[DONE]`, or a clean end of the stream, before any `finish_reason` | a bad shape: the turn never said it finished | a bad shape |
| a transport failure mid-stream | unreachable, or a timeout for `timedOut` | same |
| the idle limit or the ceiling passes | a timeout | same |
| the caller cancels | cancelled | same |

As in a whole reply, the finish reason is read before the refusal mark, so an answer that ends at `length` is truncated
even on a refused turn.

**Bounds.** The stream as a whole has no byte cap: every token comes wrapped in a chunk envelope, so a raw cap would
refuse long legitimate answers; the timeouts bound it in time instead. Each line (a comment's included), one event's
joined data, and the assembled answer (the UTF-8 bytes of its content) are each capped at the reply cap, and passing
any of them is a bad shape at the byte, or chunk, that passes it. Reasoning deltas are parsed and dropped, and each
event's parsing is released with the event, so what the provider holds stays within about four times the reply cap
however long the stream runs: the line being read, one event's data and its copy being parsed, and the answer. This
does not count bytes URLSession has received but the provider has not yet read: a server that sends faster than they
are read is buffered there, outside the provider's control. Measured through the test stand-in at 800,000 small
events sent with no pause, the process grew about 92 MB before the per-event release and about 41 MB after it, the same
as the stand-in's stream read with no provider at all. How much a real connection buffers is unmeasured.

**Known cost.** The stream is read a byte at a time, and every byte refreshes the idle watchdog's clock under a lock.
Streaming a 1 MiB answer through the test stand-in took about 300 ms in a release build, about 140 ms of it that
per-byte clock read and lock. A real answer is a small fraction of that size, so this is noted, not optimised.

**Not yet checked against a live server.** Every streamed test runs against a `URLProtocol` stand-in. How each
profiled service actually streams (a single LF between events, `finish_reason` only before `[DONE]`, or none at all,
any of which would make every answer a bad shape), and how many bytes of envelope it sends per token, is unmeasured.

## The model list

`GET <base>/<modelsPath>` with the profile's auth when the profile requires a key for it. The OpenAI shape is
`{"data": [{"id": ..., "name"?: ..., "context_length"?: ...}, ...]}`; OpenRouter adds
`architecture.output_modalities`. Parsing keeps `id`, `name` and `context_length` when present, deduplicates by `id`,
sorts by `id`, and applies the profile's filter (`.all`, or `.textOutput`: keep entries whose output modalities contain
`text`, and entries with no modality metadata). Failures are typed apart from the provider's: a profile with no list
path is `noList`, a list that needs a key and has none is `missingKey` (no request is made), 401/403 is
`unauthorised`, 404 is `noList`, other statuses carry the server's bounded words, and a 2xx that is not a list is a bad
shape.

The list is a snapshot with a time, not a promise: a service may list a model the reader's key cannot use, and only an
ask proves otherwise.

## Profiles

A profile is data: id, name, base URL, auth, whether a key is required, the preferred token field, the model-list path
and its auth requirement, the filter, hints and a note. The services measured on 2026-10-25 are OpenAI, Anthropic's
OpenAI-SDK compatibility layer, DeepSeek, Ollama, LM Studio, llama.cpp's server, Groq, OpenRouter, Google Gemini's
OpenAI compatibility, xAI and Mistral. Adding a service is a row, not code.

## Parity

The wire contract above is fixture-able: a request body, a scripted reply sequence, and the expected typed outcome. The
fixtures (`fixtures/ai/`) and the Go implementation are the next change to this component; until they land, the Swift
implementation is the only one, and the README says so. This section is replaced by the fixture list when they do.
