# WebSocket session (`NET-03`, `NET-04`)

Component `session`. Product `RealtimeConnection` (Swift), package `realtime` (Go). Fixtures: `fixtures/ws/`,
`fixtures/events/`.

## Connection (`NET-03`)

- `wss://api.openai.com/v1/realtime?model=<model>` with `Authorization: Bearer ek_…`, under the same origin policy
  and proxy as HTTP ([http-and-mint](http-and-mint.md)). A refused handshake is classified like an HTTP reply from
  its status and `x-request-id` alone: its body is never read, because URLSession does not expose it, so a 401 is
  `auth-expired` whatever error code its body names. A reply that should have upgraded (a 2xx, or a 101 that is not
  a valid upgrade) is `protocol`, with its status. The handshake timeout bounds the whole handshake, a malformed 101
  included.
- The handshake timeout and the read limit are required arguments (Mochi 4,000,000 bytes; Checkmate 2,000,000 for
  the recorder and 2 MiB elsewhere; recite 16 MiB). The size check runs before JSON decoding: a message longer
  than the limit is `protocol` and is never decoded.
- Sessions take a secret source, never a sign-in: `RealtimeSecretSource` yields a client secret for a session
  object, so a later remote client changes no API.

## Close and error classification

| Close code | Class |
|---|---|
| 1006 (connection dropped; never on the wire) | `network` |
| 1001, 1011, 1012, 1013, 1014 | `server` |
| 1008 | `invalid-request` |
| 1000, 1002, 1003, 1005, 1007, 1009 (the peer ended a session that was still expected to run) | `protocol` |
| any other | `server` |

An `error` event is classified by its `error.type` and `error.code`:

| Error | Class |
|---|---|
| code `rate_limit_exceeded` or `insufficient_quota`, or type `rate_limit_error` or `insufficient_quota` | `rate-limited` |
| code `missing_scope` | `route-denied` |
| type `invalid_request_error` | `invalid-request` |
| any other | `server` |

Independently, an error is **fatal** to the session (the session cannot continue and must be replaced) when its code
is `session_expired`, `invalid_api_key` or `session_not_found`, or its type is `authentication_error` or
`permission_error`.

## Typed events (`NET-04`)

| Type | Required fields | Result |
|---|---|---|
| `session.created` | `session.type` | session type, model |
| `response.created` | `response.id` | response id, metadata |
| `response.output_audio.delta` | `response_id`, `delta` (base64) | response id, PCM bytes |
| `response.output_audio_transcript.delta` | `response_id`, `delta` | response id, text |
| `response.done` | `response.id`, `response.status` ∈ completed, failed, incomplete, cancelled | id, status, `status_details.reason`, token usage, metadata |
| `rate_limits.updated` | `rate_limits[].remaining` | name, limit, remaining, reset seconds per entry |
| `error` | `error.type` | the classified error |
| `input_audio_buffer.committed` | `item_id` | item id |
| `conversation.item.input_audio_transcription.delta` | `item_id`, `delta` | item id, text |
| `conversation.item.input_audio_transcription.completed` | `item_id`, `transcript` | item id, transcript |
| `conversation.item.input_audio_transcription.failed` | `item_id`, `error.type` | item id, the classified error |

A documented type the package does not consume (`session.updated`, `response.output_item.added`, …; the list is in
the fixtures) is **ignored**. Any other type is **unknown**: passed through raw and counted, as API drift. A session
delivers both, each with its raw bytes, so an app can read a type the package does not decode. Invalid JSON, a
missing `type`, a field of the wrong type, a missing required field, bad base64 or an unknown `response.status` is
a `protocol` error that names the event type only, never the payload. Only the fields of a consumed type are read,
so an ignored or unknown event is never refused for the shape of a field the package does not consume.

`delta` is standard base64 as Go's `StdEncoding` reads it, padded, with padding only at the end; trailing bits are
ignored; CR and LF are refused.

## Wire JSON

Every JSON document the provider sends (an event, an error body, a mint reply) is read with one rule set, so the
implementations accept and refuse the same documents whatever their JSON libraries do:

- the document is one RFC 8259 value, with only whitespace around it, in valid UTF-8 (no byte order mark); a
  `\u` surrogate escape must be a high one followed by a low one;
- no object, at any depth, has two members with the same name after unescaping;
- nesting is at most 10,000 levels (Go's `jsontext` limit);
- member names match exactly; null is the same as absence;
- a string field must be a string; an integer field is a number written without fraction or exponent that fits a
  signed 64-bit integer; a float field is a number within the range of a double;
- a provider `code` is its string, or for any other value its exact JSON text;
- in a string map (`metadata`), two names that are canonically equivalent (`\u00e9` and `e\u0301`) are invalid,
  because Swift's `String`-keyed map cannot hold both; so is a name with a run of more than 30 combining marks
  (non-starters, counted in each character's canonical decomposition), the stream-safe bound beyond which Go's
  normaliser inserts U+034F and the two implementations would compare names differently;
- a field of the wrong type in a consumed event is a `protocol` error that names the event type.

Go reads through `encoding/json/jsontext`; Swift has its own validator, because `JSONSerialization` folds none of
these the same way.

Not in the contract: pooling, recycling, out-of-band request shape, instructions, turn watchdogs, renewal.
