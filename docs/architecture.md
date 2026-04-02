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

### llama-answer, llama-rewrite, llama-embeddings

Three dedicated llama.cpp server containers, one per role.

- `llama-answer`: streams grounded answers and drives the main chat pipeline.
- `llama-rewrite`: rewrites search queries to improve SearXNG results.
- `llama-embeddings`: generates document and query embeddings for reranking.

Each container watches its role-specific model file (`current-model.txt`, `current-rewrite-model.txt`, `current-embedding-model.txt`) and reloads the inference process when the file changes.

## Search workflow

1. User submits a query to `POST /search`.
2. Backend creates a conversation thread and stores the initial user message.
3. Backend fires the original query to SearXNG immediately.
4. In parallel, a rewrite model produces a stronger search query.
5. The rewritten query is also sent to SearXNG and the two result sets are merged into raw results.
6. Each newly stored URL is fetched and cleaned with `trafilatura` as soon as it is added.
7. Extracted texts are embedded and persisted.
8. The backend reranks sources by cosine similarity against the rewritten-query embedding.
9. A final answer model streams a grounded answer from the top reranked sources and cites them.
10. HTMX refreshes the result and pipeline blocks while the initial answer stream is rendered in real time.

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
