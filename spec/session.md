# WebSocket session (`NET-03`, `NET-04`)

Component `session`. Product `RealtimeConnection` (Swift), package `realtime` (Go). Fixtures: `fixtures/ws/`,
`fixtures/events/`.

## Connection (`NET-03`)

- `wss://api.openai.com/v1/realtime?model=<model>` with `Authorization: Bearer ek_…`, under the same origin policy
  and proxy as HTTP ([http-and-mint](http-and-mint.md)). A refused handshake is classified like an HTTP reply.
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
the fixtures) is **ignored**. Any other type is **unknown**: passed through raw and counted, as API drift. A session delivers both, each with
its raw bytes, so an app can read a type the package does not decode. Invalid
JSON, a missing `type`, a missing required field, bad base64 or an unknown `response.status` is a `protocol` error
that names the event type only, never the payload.

Not in the contract: pooling, recycling, out-of-band request shape, instructions, turn watchdogs, renewal.
