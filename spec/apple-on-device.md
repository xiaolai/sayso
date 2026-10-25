# Apple's on-device model as a provider

`AppleOnDeviceProvider<Answer>` (Swift: `SaySoAIApple`, macOS 26.4 / iOS 26.4; framework-bound, no Go equivalent).
Answers one composed question with Apple's `FoundationModels` system model, guided into the caller's own `Generable`
answer type. This contract covers availability, generation and error mapping; it does not interpret the answer —
the caller's type and encoder decide what one is.

**Threat model.** Nothing leaves the device: the model runs in-process, with no network call this library makes.
The only external input is the composed question, which never reaches a log (see `diagnostic`, below).

## Availability, reported not assumed

`AppleOnDeviceProvider.availability()` maps `SystemLanguageModel.default.availability` to this library's own closed
set, `AppleAvailability`, because the framework's own enum is neither `Codable` nor `CaseIterable` and may grow a
case the library does not yet know:

| Case | Meaning |
|---|---|
| `available` | the model can run now |
| `deviceNotEligible` | this hardware cannot run it |
| `appleIntelligenceNotEnabled` | the system feature is off |
| `modelNotReady` | still downloading or otherwise not staged |
| `system(required:running:)` | the running system is older than the caller's own floor for this source (never produced by this library itself; a case the app's own version check may construct) |
| `unknown` | a reason this build does not recognise, or (pre-26.4 SDKs) the only answer there is |

`AppleAvailability.fixedCases` lists the five cases with no payload, for a settings row that must word each one by
name; `.system` is excluded because its versions are the reader's, never a fixed set. Only a truly unrecognised
framework value, mapped through `@unknown default`, is `.unknown`.

A caller decides for itself whether the running system is new enough for its own use of this source; that floor and
its sentence belong to the app, not to this library.

## Answering

Each ask is one fresh `LanguageModelSession`, `temperature: 0`, with `guardrails: .permissiveContentTransformations`
on the model. No history crosses asks. Before a session is created:

1. **Cancellation** (`Task.isCancelled`) is checked first; a cancelled ask never reaches the framework.
2. **Availability** is checked again, fresh: `.unavailable(reason)` throws `.unavailable` with the mapped reason,
   before a session exists.
3. **A token preflight** compares the question against `model.contextSize`. A question whose own count is past the
   window can never fit, and is refused as `.contextExceeded` before a session exists. The count leaves out the
   answer schema and the output budget, so a question under the window proves nothing, and a count that cannot be
   taken is never a refusal; the framework's own context error (below) decides every other case.

Generation is **guided**: `session.respond(to:generating: Answer.self, options: .init(temperature: 0))`, not a
free-form prose ask that happens to decode as JSON — guided generation was measured to fail a strict shape check far
less often. The response's `content` (the caller's `Answer`) is handed to the caller's `encode` closure, and that
string is the provider's answer; this library never inspects it.

Cancellation is checked again twice more: once the token count returns, and once the response arrives — an answer
that comes back after the caller stopped caring is discarded, never returned.

## Error mapping

Two framework error enums carry the same shapes on different systems: `LanguageModelSession.GenerationError` (macOS
26) and `LanguageModelError` (macOS 27, additive). Both are checked, in that order, so a build running on either
system is mapped the same way:

| Framework case (either enum) | This library's error |
|---|---|
| context window exceeded | `.contextExceeded` |
| rate limited, concurrent requests | `.rateLimited` |
| guardrail violation, refusal | `.guardrail` |
| anything else recognised, or `@unknown default` | `.framework("generation")` |
| not one of the two enums at all | `.framework("generation")` |

The diagnostic string is always the fixed literal `"generation"` — never the framework's own sentence and never the
error's type name, because either could echo the reader's question back into a log. The error's `code` (see
[`spec/chat-completions.md`](chat-completions.md#answers-and-refusals)'s sibling, `AIProviderError.code`) is what a
log line carries; the sentence a reader sees is the app's own, worded from the typed case.

## The platform without the framework

A build whose SDK has no `FoundationModels` (an older OS, or a target this library does not guide on) still
compiles `AppleOnDeviceProvider<Answer>`: `availability()` returns `.unknown` and every `answer(to:)` throws
`.unavailable(.unknown)` (`.cancelled` when the task is already cancelled), so a consumer written against the fuller build needs no `#if canImport` of its own.

## Parity

Framework-bound: there is no Go implementation and none is planned. The test suite
(`Tests/AIProviderAppleTests/`) checks availability reporting and the typed-refusal path unconditionally; a real
generation runs only when the caller opts in by name (`SAYSO_AI_APPLE=1`), so no test spends a quota or depends on
this Mac's model being ready.
