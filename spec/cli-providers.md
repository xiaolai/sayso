# The reader's CLI as a provider

`ClaudeCLIProvider` and `CodexCLIProvider` (Swift: `SaySoAICLI`, macOS only; no Go equivalent — the sign-in each CLI
reads is process-bound). Each answers one composed question by running the reader's own `claude` or `codex` once and
reading its stdout. This contract covers what both do the same way; each CLI's own flags and envelope are its own
section.

**Threat model.** The CLI runs with none of the reader's configuration: no tools, no MCP servers, no hooks, no
instructions, no session on disk. The prompt arrives on the child's stdin, never as a shell argument or in an
environment variable; the app's own folders hold the sign-in and nothing else; no sign-in is ever read, written or
forwarded by this library.

## One child, boxed

Every ask is `CLIProcess.run`: one process in a process group of its own, started with an explicit argument array (no
shell parses them), an explicit environment (nothing the host app happens to hold leaks in unless this contract says
so), and an app-owned working directory. Both output streams are capped (1 MiB stdout, 64 KiB stderr) so a chatty or
hostile child cannot grow the app's memory; stderr is read only for classification, never logged. The run owns its
process group: ending it (deadline, cancellation or the launcher's own exit) kills every descendant rather than
leaving one attached to the pipes.

The child's environment is always: `HOME` and `TMPDIR` from the host process, a `PATH` repaired with the standard
installer folders (`/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin`, an inherited `PATH` prepended when there is one —
an npm-installed CLI needs `node` beside it), plus each provider's own folder variable. No `ANTHROPIC_*` or other
credential the host app holds is ever added.

## Discovery and the version floor

An explicit executable is used as given, or refused (`notInstalled`) if it does not resolve to a regular,
this-process-executable file. Otherwise discovery searches `PATH` then the GUI candidates (`~/.local/bin`,
`~/.npm-global/bin`, `~/.volta/bin`, `~/.bun/bin`, `~/Library/pnpm`) — a Finder-launched app has no login-shell `PATH`.

Both providers refuse (`versionTooOld`) below a measured floor: `claude` **2.1.304** (the oldest build with
`--json-schema` and `--safe-mode`), `codex` **0.163.0** (the oldest with `--ephemeral`, `--ignore-user-config`,
`--ignore-rules`, `--output-schema`). The floor is what was measured to have the flags, not a guess at an earlier
version that might also work. The version string is read from `--version` (Claude) or the sign-in probe (Codex); its
first digit-leading token is the version, so a prefixed form (`codex-cli 0.163.0`) still parses. Components compare
as integers; a missing or non-numeric component is `0`.

## The app's own folders

Before any ask, every app-owned folder a provider needs (its config/home directory, its working directory) is
created private (`0700`) if missing, then checked: not a symlink, owned by this user, and not group- or
world-writable. The path to it is checked before creation and after: every folder on the way, symlinks followed,
is root's or this user's and writable by nobody else (a sticky folder such as `/tmp` excepted), and every symlink on
the way is root's or this user's. A folder that fails the check is `.framework("folder")`, never silently chmod'd —
a folder the app did not make is not one it repairs.

## The budget

One `Duration` covers the whole ask, probe included: `check()` and `answer(to:)` share one deadline, so what a
version check or sign-in probe spends is what the ask has left; once nothing is left, `.timeout`. Claude's default is
120 s, Codex's 180 s; Codex's `login()` has its own 300 s deadline, since it waits on the reader's browser.

A cancelled ask throws `.cancelled` at every check point — before the child starts, after preparation, and after the
child exits but before its status or answer is read, so neither an answer nor a failure that arrives after the caller
stopped caring is handed back.

## Classifying a failure

A non-zero exit, or an envelope that says it is an error, is classified from the CLI's own words (stdout and stderr,
or the structured envelope's `result`), against three narrow marker lists, checked in order:

1. **Sign-in** ("not signed in", "please run /login", "please log in", "authentication required", "invalid api
   key", "unauthorized", plus `codex login` for Codex) → `.notSignedIn`.
2. **Throttled** ("rate limit", "usage limit", "quota exceeded", "too many requests") → `.rateLimited`.
3. **Refused** ("model refused", "refused to answer", "model declined", "declined to answer", "violates our/the
   usage policy", "violates our/the content policy", "blocked by the safety filter", "blocked by the content
   filter") → `.refused`. "connection refused" and "refused to connect" are transport words, not model words, and
   are deliberately excluded; a policy or a filter named alone ("failed to initialize safety filter") is a broken
   CLI, not a refusal.

Nothing else is claimed as one of these: a failure that matches none is `.framework("<provider> exit <status>")`, or
`.framework("<provider> error")` for an in-envelope error with no process exit (status `0`).

## Claude: the one-shot ask

`claude -p --output-format json --json-schema <schema> --no-session-persistence --tools "" --strict-mcp-config
--setting-sources "" --disable-slash-commands --permission-prompts none --safe-mode [--model <model>]`, the question
on stdin. `--bare` is deliberately never used: it would skip the reader's own OAuth, which is the point of this
provider.

The caller supplies `answerSchema`: a JSON Schema handed to `--json-schema`, read back from the envelope's
`structured_output` (any JSON value, re-encoded with sorted keys) or, failing that, its `result` string. This
library does not interpret the answer; the schema and its meaning are the caller's. An envelope with `is_error: true`
or a `subtype` other than `"success"` is a refusal, classified from the envelope's own `errors` (with `result` as a
fallback), whatever else it carries (`structured_output` included) — the structured field is never read past a
refusing envelope. When those words name nothing, a failing exit is classified from the streams instead. A success
envelope whose `stop_reason` is `"refusal"` is `.refused`, and `"max_tokens"` is `.truncated`.

Claude Code owns its own credential, written to the Keychain under the service name it derives from
`CLAUDE_CONFIG_DIR`; this library only sets that variable to an app-owned folder and never reads `~/.claude` or any
keychain item itself.

## Codex: the one-shot ask and the documented login

`codex exec --skip-git-repo-check --ephemeral --ignore-user-config --ignore-rules --disable shell_tool --disable
browser_use -s read-only -C <workingDirectory> -c approval_policy="never" -o <messageFile> [-m <model>] -`, the
question on stdin. Both tool sets are disabled (a read-only sandbox alone would still let the model run shell
commands) so the only thing text in the prompt could ask the model to do is answer; the answer is read from
`<messageFile>`, bounded while reading, never loaded past the stdout cap.

The sign-in is read through `CodexAuth` (see [`spec/credentials.md`](credentials.md)), probed in the same restricted
environment the ask runs in — a probe using the host app's full environment could honour a proxy or credential
variable the ask never sees. Codex is the probe's only source (a sign-in file left without the CLI is `noCodex`), the
probe runs within the ask's deadline and the run gets only what it left, and the binary run is the one probed: a
probed binary that is gone is `.notInstalled`, never replaced by discovery. `CodexAuth`'s states map onto this contract's errors: `noCodex` → `.notInstalled`,
`codexTooOld` → `.versionTooOld`, `notSignedIn`/`noSignInFile`/`expired` → `.notSignedIn`,
`apiKey`/`unsupportedAuth`/`appServerAuthUnavailable`/`providerWithoutOpenAIAuth` → `.unsupportedAuth`, `codexError` →
`.framework("codex")`. `check()` always probes a freshly built client (a sign-out moments ago must not read as ready
from a cached token); `answer(to:)` uses the long-lived one. `login()` runs the CLI's own documented `codex login`,
streaming each line of either output stream to the caller as it arrives (the URL and code the reader needs), and
replaces the long-lived client afterward so the next ask's state cannot be the signed-out one's.

This library writes no credential file itself and discards the fetched token unread; the sign-in step is entirely
the CLI's.

## Parity

Process-bound by nature: there is no Go implementation and none is planned (`go/aiproviders` covers the HTTP
provider only). Coverage is the Swift test suite (`Tests/AIProviderCLITests/`), which stubs the child process rather
than running a real CLI except where a test explicitly opts in by name.
