# Architecture

## Services

### auth-proxy

- Public entrypoint.
- Uses OAuth2 Proxy.
- Enforces authentication and forwards `X-Forwarded-User` to the backend.

### backend

- Go HTTP service.
- Serves HTML templates and static assets.
- Owns SQLite persistence.
- Calls SearXNG internally with `GET /search?q=<query>&format=json`.
- Orchestrates page fetch, extraction, summarization, conversation storage, and memory refresh.

### searxng

- Internal-only metasearch engine.
- No public port published by Compose.

### llama-answer, llama-embeddings

Two dedicated llama.cpp server containers, one per role.

- `llama-answer`: streams grounded answers, drives the main chat pipeline, and optionally generates query reformulations.
- `llama-embeddings`: generates document and query embeddings for reranking.

Each container watches its role-specific model file (`current-model.txt`, `current-embedding-model.txt`) and reloads the inference process when the file changes.

## Search workflow

1. User submits a query to `POST /search`.
2. Backend creates a conversation thread and stores the initial user message.
3. Backend fires the original query to SearXNG. If `BAP_QUERY_REFORMULATIONS > 0`, the answer model also generates N alternative phrasings and each is searched in parallel.
4. All result sets are merged (deduped by URL) and stored.
5. The frontend connects to the SSE endpoint (`GET /conversations/{id}/events`) and receives real-time card and pipeline status updates.
6. Each newly stored URL is fetched and cleaned with `trafilatura`. On failure, the search snippet is used as fallback.
7. Extracted texts are embedded and persisted; the SSE stream delivers source text to the UI immediately.
8. The backend reranks sources by cosine similarity against a composite query embedding.
9. A final answer model streams a grounded answer from the top reranked sources and cites them.
10. On completion, the SSE stream sends a `close` event and the UI reloads the messages panel.

## Conversation flow

- Each search becomes a conversation.
- Follow-up chat uses:
  - persistent user memory
  - stored page summaries
  - extracted source text
  - recent conversation history
- Assistant replies are saved back into the same thread.

## User memory flow

- `user_memory` stores a compact profile per user.
- After enough user turns, the backend sends the latest transcript to the model with:
  - `Update the user memory based on the following conversation.`
- The updated memory summary is stored and injected into future prompts.

## Backend modules

- [backend/main.go](../backend/main.go): config, bootstrap, routes
- [backend/search.go](../backend/search.go): SearXNG client
- [backend/fetch.go](../backend/fetch.go): concurrent fetch and trafilatura extraction
- [backend/summarize.go](../backend/summarize.go): async summary workers
- [backend/conversation.go](../backend/conversation.go): SQLite persistence and chat handlers
- [backend/memory.go](../backend/memory.go): persistent memory refresh
- [backend/logging.go](../backend/logging.go): JSON logging and request context
- [backend/llm.go](../backend/llm.go): llama.cpp chat integration
- [backend/events.go](../backend/events.go): SSE event broker (pub/sub per conversation)
- [backend/models.go](../backend/models.go): model discovery, role assignment helpers, and download

## Database schema

Main tables:

- `users`
- `conversations`
- `messages`
- `search_results`
- `summaries`
- `user_memory`

The canonical schema lives in [database/schema.sql](../database/schema.sql).

## Low-resource design choices

- SQLite instead of a separate database service.
- HTML templates and HTMX instead of a frontend framework.
- Small worker pools for fetch and summarization.
- Only top 3 URLs summarized by default.
- Extracted text truncated before prompting.
- llama.cpp model kept warm in a dedicated container.
- Internal-only search and inference services.
