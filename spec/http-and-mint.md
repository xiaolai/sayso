# HTTP and minting (`NET-01`, `NET-02`, `NET-05`)

Component `mint`. Product `RealtimeConnection` (Swift), packages `openaihttp` and `realtime` (Go).
Fixtures: `fixtures/mint/`.

## Origin policy (`NET-01`)

A request carrying `Authorization` is sent only to `https://api.openai.com`: scheme `https`, host `api.openai.com`,
port empty or 443, no userinfo, no `Host` override. Anything else is refused before a connection is made, as class
`internal` (a programming error). Redirects are never followed: a 3xx reply is delivered as it came and classified
like any other reply, `protocol` with its status, request id and provider fields, whatever its `Location` (which is
never quoted, and may be malformed). No credential is added by the HTTP stack: stored credentials and
authentication challenges other than server trust are refused, so only the `Authorization` the policy checked is
sent. The proxy comes from the
environment: in Go the `HTTPS_PROXY` family, on Apple platforms the system proxy settings `URLSession` reads.
Tests reach a fake server only through an origin injected explicitly as a test origin (Swift: loopback only).

## Minting (`NET-02`)

- `POST /v1/realtime/client_secrets`, `Authorization: Bearer <ChatGPT token>`, `Content-Type: application/json`.
- The destination is checked against the origin policy before the credential source is asked for a token: a wrong
  base URL is refused as `internal` without costing a sign-in (a Codex start, perhaps a refresh).
- Body: `{"session": <caller's session object>, "expires_after": {"anchor": "created_at", "seconds": <caller's
  lifetime>}}`. The session shape is app policy; the package passes it through unchanged.
- Reply: at most 1 MiB; JSON with `value`, a non-empty string of at most 4,096 UTF-8 bytes, and `expires_at`, an
  integer (Unix seconds), read with the [wire JSON](session.md#wire-json) rules. Anything else is `protocol`, with
  the reply's status and request id. The secret is registered with the scrubber before it is
  returned, with its expiry kept as that exact integer (beyond 2^53 a double cannot hold it).
- **`auth-expired` once.** On an `auth-expired` reply the minter asks the credential source to refresh after that
  token ([credentials](credentials.md)) and retries once. A second `auth-expired` is returned as `auth-expired`,
  with the second refusal's status and provider fields.
- Timeouts are required arguments (Mochi 45 s request / 90 s resource, Checkmate 20 / 25 s, recite 20 s).
- Retry policy is not in the contract.

## Classification (`NET-05`)

| Reply | Class |
|---|---|
| 401 with error `code` `missing_scope` | `route-denied` |
| 401 | `auth-expired` |
| 403, 404 | `route-denied` |
| 429 | `rate-limited` |
| 500 and above | `server` |
| other 4xx | `invalid-request` |
| 1xx, 3xx | `protocol` |
| a transport failure (DNS, TLS, refused, reset, timeout) | `network` |
| cancelled by the caller | `cancelled` |

The other classes are `auth-mode` and `codex-error` (credentials), `internal` (programming errors). Every error
carries the HTTP status, the provider's `type`, `code` (a value that is not a string is kept as its exact JSON text)
and `param`, and the `x-request-id` header, all scrubbed. At most 64 KiB of an error body is read, with the
[wire JSON](session.md#wire-json) rules; a body that breaks them, or has a field of the wrong type, yields no fields. The provider's free-text `message`
is kept scrubbed for diagnostics and is never meant for display. Checkmate's model fallback decides on
`status ∈ {400, 404, 422}` itself, from the status on the error.
