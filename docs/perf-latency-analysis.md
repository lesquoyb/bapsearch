# Latency analysis — search → results, and results → answer

> Status: investigation / design notes. No behaviour changed yet.
> Branch: `claude/search-latency`. Scope: understand why the two latencies grew
> (they were fine early in the project) and lay out a prioritized fix plan.

## TL;DR — the three headline findings

1. **Embeddings fail on most pages (the "90 % of sites" bug) because truncation
   is best-effort, not guaranteed.** `truncateToTokens` (`backend/llm.go:489`)
   relies on a live `/tokenize` round-trip and **returns the *full* untruncated
   text on any hiccup** (bad URL, unreachable endpoint, unexpected JSON, non-2xx
   — `llm.go:514,520,525,530`). When that happens the whole extracted page
   (up to `max_extract_chars` = 12 000 chars ≈ 3–4k tokens, `fetch.go:177`)
   is POSTed to an embeddings server whose context is **2048** and whose
   physical batch (`--ubatch-size`) is **512** in the shipped `.env` examples.
   llama.cpp then rejects the input ("input too large…") and the source is
   marked `error`. The `max_embedding_tokens` knob can't save you because it's
   only applied *through* that same fragile path — hence "no matter how we set it".

2. **Time-to-first-results regressed because heavy work sits in front of the raw
   results.** Two causes: (a) when `query_reformulations > 0`, an **answer-model
   LLM call runs *before* any SearXNG query** (`summarize.go:95‑104`); (b) the
   whole pipeline runs on a **single worker** (`BAP_SUMMARY_WORKERS` default
   `1`, `main.go:708`), so a new search queues behind the slow fetch/embed of a
   previous one.

3. **Time-to-answer is dominated by serial embeddings + chain-of-thought.**
   `embedding_batch_size` defaults to `1` (`main.go:266`) so every document is
   embedded one HTTP call at a time, *and* each embed makes up to ~9 extra
   serial `/tokenize` calls (the binary search, `llm.go:536‑559`). On top of
   that `enable_thinking` defaults to `true` (`settings.go:141`), so every
   grounded answer does a full reasoning pass before the first visible token.

All three line up with commit **`daea897`** ("Redo the rewrite by parallel
research of paraphrases **+ truncate the number of tokens used for embeddings to
avoid errors**"), which introduced both the reformulation step and
`truncateToTokens`. The embedding-size problem it tried to fix is still not
fixed reliably.

---

## How to measure (before changing anything)

The app already logs durations to the mounted volume — use them to quantify
each phase instead of guessing:

- `llm.jsonl`:
  - `embedding_response` → `duration_ms`, `input_chars`, `vector_dim` (success)
  - `embedding_request_status` / `embedding_batch_request_status` → failures with
    the server's body. **Counting these confirms the 90 % figure.**
- `backend.jsonl`:
  - `pipeline_job_started` / `pipeline_job_finished`
  - `url_fetched` (`html_bytes`), `text_extracted` (`text_bytes`)

Quick triage commands (on the logs volume):

```bash
# embedding failure rate
grep -c embedding_request_status llm.jsonl
grep -c embedding_response       llm.jsonl
# slowest embeds
grep embedding_response llm.jsonl | jq -r '.duration_ms' | sort -rn | head
```

---

## Pipeline A — search start → raw results shown

Path: `handleSearch`→`startSearch` (`conversation.go:147`) enqueues a
`SummaryJob` and 303-redirects. The browser loads the conversation page; the
results panel does `hx-get .../results` and otherwise waits for the SSE
`results` event. So **results appear only once the worker has run the SearXNG
search and persisted rows** (`summarize.go:113‑155`).

What sits on that critical path:

| Step | Cost | Notes |
|---|---|---|
| Queue wait | 0 … *(whole previous job)* | **single worker**, `main.go:708` |
| Reformulation LLM call | 0 … several s | only if `query_reformulations>0`; uses the **answer model** (`summarize.go:97`, `llm.go GenerateQueryReformulations`) and runs **before** SearXNG |
| SearXNG search | network | retries up to 4× w/ backoff on transient errors (`search.go:55‑73`); waits for SearXNG's own engine aggregation |
| Persist + `results` SSE | small | `summarize.go:148‑155` |

Regression drivers: reformulations (commit `daea897`) and the single worker
becoming a bottleneck as per-job work grew.

## Pipeline B — raw results → ranked sources → grounded answer

Path: `processResults` (`summarize.go:244`) for every inserted URL: fetch →
trafilatura extract → embed; then query embed + cosine rank
(`summarize.go:194‑233`); then status `ready`. The answer itself streams
separately via `handleConversationAnswerStream` once the client asks.

| Step | Cost | Notes |
|---|---|---|
| Fetch | ≤20 s/URL | concurrent, `FETCH_WORKERS`=3 (`fetch.go:50`) |
| Extract | Python startup **per URL** | trafilatura spawned as a subprocess each time (`fetch.go:164`) |
| Embed docs | **serial** | `embedding_batch_size`=1 (`main.go:266`); each embed also does ~9 `/tokenize` calls |
| Embedding failures | wasted work | oversized inputs error out (see finding #1) → few ranked sources |
| Query embed | 1 call + tokenize | `summarize.go:194` |
| Grounded answer | **dominant** | LLM stream, `enable_thinking` default true → CoT first (`settings.go:141`) |

---

## Root cause #1 in detail — why embeddings fail "no matter the setting"

`truncateToTokens(text, maxTokens)` (`llm.go:489`):

```
embeddingsURL ──derive──> baseURL + "/tokenize"
POST {content: text}
  err / non-2xx / bad JSON ............ return text   // ⚠ FULL text, untruncated
  tokens <= maxTokens ................. return text   // fine
  tokens >  maxTokens ................. binary-search via ~9 more /tokenize POSTs
```

Failure modes that all end in "oversized input sent":

- The embeddings container doesn't answer `/tokenize` the way expected (wrong
  derived URL, server build without the route, different JSON) → silent
  fall-through to full text.
- Even when it *works*, `max_embedding_tokens` is **not clamped to the server's
  limits**. The presets set it to `1024` and `2048` (`settings.go:62,78`) while
  the shipped embeddings server runs `--ubatch-size 512` / `ctx 2048`
  (`docker/.env.example:55`). llama.cpp rejects a single embedding input larger
  than the physical batch, so `1024`/`2048` token inputs fail outright.
- A failed embed marks the source `error`; ranking then has few/no vectors, so
  the answer is grounded on a handful of sources (or none).

### Fix direction (P0)

1. **Guarantee an upper bound before the network call.** Hard-cap the text by
   characters first — `min(maxEmbeddingTokens, serverCtxBudget) × ~4 chars` —
   so an embed is *never* sent oversized, regardless of `/tokenize`. Treat
   `/tokenize` as an optional refinement, not the only guard.
2. **Clamp `max_embedding_tokens` to the server budget** (ctx and ubatch), and
   surface the effective value. Optionally make the embeddings server's
   `--ubatch-size` ≥ `ctx` so any input up to ctx is accepted.
3. **Degrade gracefully on a size error**: retry the embed with half the text
   instead of dropping the source.
4. **Drop the per-embed binary search** (~9 serial round-trips × every doc ×
   every query) — it's both the failure surface and a latency tax.

This single area should move both the success rate ("90 % fail") *and* the
ranking/embedding latency.

---

## Prioritized recommendations

Legend — Impact / Effort / Risk (L/M/H).

### P0 — embedding reliability (the 90 % bug) — biggest win
- **R1** Deterministic char-cap before embedding + clamp to server budget.
  `llm.go truncateToTokens/EmbedText/EmbedTexts`. *Impact H · Effort M · Risk L*
- **R2** Graceful retry-with-half on size errors instead of `error`.
  `summarize.go processResults`. *H · M · L*
- **R3** Remove the binary-search `/tokenize` loop from the hot path. *M · L · L*

### P1 — time-to-first-results
- **R4** Never block raw results on reformulations: fire the original-query
  search immediately, publish `results`, then reformulate + fan-out + merge in
  the background. `summarize.go runJob`. *H · M · M*
- **R5** Split the pool so the cheap "get raw results" step can't queue behind a
  heavy fetch/embed job — or bump `BAP_SUMMARY_WORKERS` default 1→2/3
  (`main.go:708`). *M · L · L*

### P1 — time-to-answer
- **R6** Default `embedding_batch_size` > 1 (e.g. 8, aligned with ubatch) and/or
  embed documents concurrently. *H · M · M*
- **R7** Make `enable_thinking` default **false** at the global level (the
  capable presets already turn it on, `settings.go:60,76`); CoT on every answer
  is the single biggest answer-latency cost on small hardware. *H · L · M*
- **R8** Lower `max_extract_chars` default (12 000) and/or chunk: smaller inputs
  embed and answer faster. *M · L · L*

### P2 — structural
- **R9** Run trafilatura as a long-lived worker/HTTP service instead of spawning
  a Python process per URL (`fetch.go:164`). *M · M · M*
- **R10** UTF-8-safe extract truncation (`fetch.go:178` slices raw bytes). *L · L · L*

---

## Suggested next step

Start with **R1–R3** behind the existing settings: it's the highest-impact,
lowest-risk change, directly targets the reported "extraction fails on 90 % of
sites", and removes a chunk of embedding latency at the same time. Measure
`embedding_request_status` count before/after to verify. Then tackle R4/R5 for
time-to-results.

I can implement any of these on this branch on your go-ahead — R1–R3 is the
natural first PR.
