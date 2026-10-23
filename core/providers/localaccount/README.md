# Local account providers

`localaccount.Provider` implements the two native provider identifiers `chatgpt` and `codebuddy`. Shared protocol converters remain in `openai` and `schemas`; account credentials are independent of `Key.Value` and the machine OAuth grant configuration.

The HTTP transport supplies its application data directory. Go SDK hosts can supply `LocalAccountConfig.DataDir`; without it, account metadata defaults to `~/.config/bifrost/accounts`. ChatGPT credentials are owned by Bifrost. CodeBuddy credentials remain in the selected desktop file. Management and inference return no tokens. OAuth grants are validated before replacing the active credential.

## Request-flow cost

The credential-mode decision is computed once when each worker starts. Requests read one boolean in place of the original `providerRequiresKey` call. Ordinary API-key providers do not instantiate a local-account manager, inspect credential files, run compatibility rules, or allocate account state.

Measured on 2026-10-23, Go 1.27.3, darwin/arm64, Apple M3 Pro, using `-benchmem -count=3`. Medians below are component microbenchmarks, not a full gateway throughput measurement.

| Component | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| Original ordinary-provider key gate | 1.732 | 0 | 0 |
| New precomputed ordinary-provider key gate | 1.733 | 0 | 0 |
| Account credential cache hit | 40.34 | 0 | 0 |
| CodeBuddy compatibility disabled, no tools | 12.07 | 0 | 0 |
| CodeBuddy compatibility enabled, unchanged short system text | 5984 | 0 | 0 |
| Chat request to neutral Responses conversion | 131.9 | 640 | 6 |
| Chat request through plan wire conversion | 410.1 | 1249 | 9 |
| Collect and finalize one chat event | 6463 | 9415 | 81 |

The ordinary-provider gate's before/after ranges overlap (1.730–1.758 and 1.732–1.735 ns/op). Compatibility and aggregation costs are paid only by the new providers. The enabled compatibility benchmark scans the reference term list; matching replacements, tools, and larger messages add work. The collector benchmark includes event marshaling, decoding, final assembly, and final decoding, rather than only a string append. Per-event bytes vary slightly with pooled encoder buffers.

The DeepSeek Harness identity rule is confined to the first system text line on the CodeBuddy wire, independent of model ID, behind `prompt_compat`. It replaces an existing identity rather than adding a prefix. On 2026-10-24, the same compatibility benchmark before/after this rule measured medians of 12.70/12.92 ns (disabled) and 6230/6641 ns (enabled, unchanged system), with 0 B/op and 0 allocs/op in both cases. The new matching multiline identity benchmark measured 15601 ns/op, approximately 230 B/op and 4 allocs/op, including the existing compatibility scan and copy-on-write conversion. It retains one fixed compiled identity regexp for the process lifetime, with no new cache or per-credential state. Native OpenAI and DeepSeek providers never call this converter.

To reproduce:

```bash
go test ./core ./core/providers/localaccount -run '^$' -bench BenchmarkLocalAccount -benchmem -count=3
```

## Retained memory and lifetime

- Each configured provider owns one current session. Tokens and desktop credential documents are read with a 1 MiB file/response cap. Desktop-file changes are checked at most once every five seconds on the request path; reconnect forces a fresh read.
- Each provider caches one account-scoped model directory for five minutes, at most 4,096 model records from a response capped at 1 MiB. The cache key is the opaque account identity. Connection changes and provider retirement clear the cache.
- Unary collectors live in a provider-owned `sync.Map` keyed by RequestID (with an internal suffix for concurrent ID collisions). Each collector accepts at most 32 MiB of serialized events; this bounds content, maps, and tool fragments. Final serialization can temporarily allocate another representation. Concurrent collectors are bounded by active provider requests and the configured worker/queue limits. Every return path deletes its entry; cancellation closes the upstream stream.
- The existing OpenAI stream helpers own SSE readers and conversion state. New stream content is never stored in `BifrostContext`; the context carries existing IDs, booleans and tracer handles only.
- A manager retains at most one login attempt. It times out after five minutes. Disconnect, provider replacement/removal and shutdown cancel its listener and release cached sessions, model data and idle HTTP connections. Credential files survive ordinary provider retirement so restart can resume the account.
- Cold credential loads/refreshes and writes serialize through a process-local mutation mutex, including overlapping provider rebuilds. CodeBuddy additionally checks the complete on-disk document before writing a refreshed token, refusing a concurrent desktop update. This does not claim a cross-process filesystem CAS guarantee.

## Verification

The local tests use temporary fake credentials and mock upstream HTTP endpoints. They cover ID-token signatures, issuer/audience/nonce/expiration, callback validation and replay, listener conflicts and cleanup, refresh coalescing, desktop write conflicts, fractional desktop timestamps, token redaction, protocol conversions, streaming, quota/revocation/truncation, tool results, images, reasoning, model-cache invalidation and collector cancellation.

The changed core, schema, config-store, pricing, transport handlers, integration and server packages passed their complete unit suites. The canonical `make test-core PROVIDER=localaccount PATTERN=LocalAccount` run passed 33 tests with no skips. After adding DeepSeek Harness first-line compatibility, the canonical scoped suite passed 52/52 tests with no skips. Targeted account and reload tests also passed `-race`. UI compilation/typechecking and the two mocked Playwright account flows passed. An isolated gateway with a local mock upstream passed twelve HTTP ingress combinations: both providers × three API formats × streaming/unary.

The provider harness's `local account` folder contains the same twelve combinations for manual real-account validation. Its collection was augmented and structurally filtered successfully: six ChatGPT and six CodeBuddy requests. Set `chatgptModel` and `codebuddyModel` to available models before the live run. Live provider/paid calls were not executed during implementation.
