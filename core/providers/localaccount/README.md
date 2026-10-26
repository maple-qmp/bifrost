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

Removing the DeepSeek Harness identity rewrite removes the first-system-line regexp check. On 2026-10-27, `BenchmarkLocalAccountCompatibility` measured medians before/after of 12.64/14.46 ns/op with compatibility disabled and 6795/6283 ns/op with compatibility enabled for unchanged system text. Both had 0 allocs/op; the disabled difference is benchmark noise because that branch is unchanged. No retained state is added.

The exact DeepSeek Harness Web GUI quote rule is checked only on CodeBuddy system text while compatibility is enabled. On 2026-10-27, `BenchmarkLocalAccountCompatibility` measured 6530/6610 ns/op before/after for unchanged short system text, with 0 B/op and 0 allocs/op in both cases. `BenchmarkLocalAccountDeepSeekHarnessGUIQuotes` measured 32347/32747 ns/op, 2/258 B/op, and 0/4 allocs/op before/after for a matching two-line system text. The extra allocations occur only when the exact quote phrase is rewritten. The rule retains no per-request or per-credential state.

Adding `DeepSeek` to the CodeBuddy system-text term matcher was measured on 2026-10-27 with `-benchmem -count=3`. The unchanged short-system benchmark measured 6352/6654 ns/op and 0/0 allocs/op before/after. The matching GUI text benchmark measured 31110/32743 ns/op, 258/650 B/op, and 4/10 allocs/op; it now performs both the quote rewrite and the term insertion. The disabled path remains allocation-free, and the compiled regexp is process-wide with no new retained per-request state.

The exact first-line removal and GUI line-prefix fix were measured on 2026-10-27 with `-benchmem -count=3`. Unchanged short system text measured 6509/6538 ns/op before/after, with 0 B/op and 0 allocs/op. A matching first-line prompt measured 18948/6773 ns/op, 378/97 B/op and 9/3 allocs/op because the identity line no longer reaches the term matcher. The GUI example measured 32438/32933 ns/op, 650/653 B/op and 10/10 allocs/op. The extra check uses fixed string prefixes, retains no state, and is skipped entirely when `prompt_compat` is disabled.

The 2026-10-24 ChatGPT tool-declaration fix adds the required `role: "developer"`
to generated `additional_tools` input items. On the same machine, median
`BenchmarkLocalAccountPlanToolWireConversion` before/after was 4686/5161 ns/op,
7768/8093 B/op, and 52/52 allocs/op (pooled buffer sizes vary). The no-tool
`BenchmarkLocalAccountPlanWireConversion` measured 460.4/439.6 ns/op, 1249/1249
B/op, and 9/9 allocs/op. The change is confined to ChatGPT requests with function
tools; it adds no retained state or cache.

The parallel-tool fixes were measured on the same machine with `-benchmem
-count=3`. `BenchmarkChatToResponsesParallelTools` before/after medians were
579.2/1035 ns/op, 2457/4923 B/op, and 16/31 allocs/op: the old code converted
only one of the two tools. `BenchmarkResponsesToChatToolIndex` measured
165.9/153.2 ns/op, 1016/1016 B/op, and 5/5 allocs/op. Empty chat deltas remained
at zero bytes and allocations (74.62/74.02 ns/op).

`BenchmarkLocalAccountChatToolIndexStream` includes loopback HTTP, SSE parsing,
request conversion and stream finalization. Before/after medians for no-tool
requests were 58235/61079 ns/op, 48824/49324 B/op, and 166/166 allocs/op; timing
ranges overlap and pooled buffer sizes vary. Two-tool requests measured
75549/77606 ns/op, 86931/93651 B/op, and 319/330 allocs/op. These measurements
isolate the ChatGPT dense-index adapter using a Go build overlay of its previous
implementation; both runs include the shared schema fixes. The extra state is
created only when a declared tool is emitted: one uint16-to-uint16 map entry per
tool index, plus Go map overhead and a collector handle, in the existing
provider-owned manager keyed by RequestID (collision-suffixed). Stream
finalization deletes the entry on success, error and cancellation. No new stream
state is stored in BifrostContext, and requests without tools skip these hooks.

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

The changed core, schema, config-store, pricing, transport handlers, integration and server packages passed their complete unit suites. The canonical `make test-core PROVIDER=localaccount PATTERN=LocalAccount` run passed 33 tests with no skips. Targeted account and reload tests also passed `-race`. UI compilation/typechecking and the two mocked Playwright account flows passed. An isolated gateway with a local mock upstream passed twelve HTTP ingress combinations: both providers × three API formats × streaming/unary.

The provider harness's `local account` folder contains the same twelve combinations for manual real-account validation. Its collection was augmented and structurally filtered successfully: six ChatGPT and six CodeBuddy requests. Set `chatgptModel` and `codebuddyModel` to available models before the live run. Live provider/paid calls were not executed during implementation.
