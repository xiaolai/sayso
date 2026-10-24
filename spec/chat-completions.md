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
| `temperature` | `0` |
| `stream` | `false` |
| `max_completion_tokens` **or** `max_tokens` | the answer budget (1,024 for an answer; 1 for a probe); which one goes first is the profile's preference |

Auth goes on the request according to the profile: `Authorization: Bearer <key>` for most services, a named header
(`api-key`) for Azure-style endpoints, or nothing for a local server. A key is never sent over plain `http` except to a
loopback host.

## Token-field negotiation

OpenAI's current models document `max_completion_tokens` and deprecate `max_tokens`; most compatible services document
only `max_tokens`; DeepSeek accepts `max_completion_tokens` and ignores it while honouring `max_tokens` (measured
2026-10-25). So: send the profile's preferred field first, and on **any** 400 or 422 answer **once** with the other
field. The retry is the status's, never a sentence's — a service need not name the field it refused.

## Answers and refusals

A 2xx body is the OpenAI chat-completion envelope:

- `choices` is a non-empty array; the first element has a `message` object with `content`.
- `content` is a string. `""` with `finish_reason: "length"` is a truncated answer, never a result — except for a
  probe, where an empty or length-capped content still proves the endpoint, the model and the key.
- `content: null` is accepted for a probe only (a reasoning model's one-token turn spends the budget on reasoning); an
  answer with no content is no answer.
- `finish_reason: "content_filter"` or a non-empty `message.refusal` is a refusal.

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
