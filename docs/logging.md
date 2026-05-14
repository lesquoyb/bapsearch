# Logging Strategy

bap-search writes structured JSON logs to the mounted logs volume.

Two streams in `/logs/` (overridable via env vars):

- `/logs/backend.jsonl` (`BAP_LOG_PATH`) — HTTP requests, search/fetch/extraction events, conversation and memory events. Also mirrored to stdout.
- `/logs/llm.jsonl` (`BAP_LLM_LOG_PATH`) — dedicated trace of every LLM and embedding API call: full prompts, full responses, request size, response size, latency, endpoint, call kind (`chat`, `chat_stream`, embedding batch). File-only (not duplicated to stdout) so prompt payloads don't drown the general stream.

## Required fields

Every request-scoped log entry includes:

- `timestamp`
- `request_id`
- `user_id`
- `conversation_id`

## Logged events

### HTTP requests

- method
- path
- response status
- duration in milliseconds

### Search events

- raw user query
- SearXNG failures

### Fetch and extraction events

- fetched URL
- HTML payload size
- extracted text size
- extraction failures

### LLM events (in `llm.jsonl`)

For every chat / chat-stream / embedding call:

- `call` — `chat`, `chat_stream`, or `embedding_response` / `embedding_batch_response`
- `endpoint` — the upstream llama.cpp URL
- `streaming`, `reasoning`, `max_tokens`, `message_count`
- `prompt` — full serialized prompt (role-prefixed messages)
- `response` — full model response text
- `response_chars`, `reasoning_chars`, `duration_ms`, `status_code`
- For batched embeddings: `batch_size`, `vector_dim`

Inference errors are logged at `ERROR` level with the same correlation fields plus the upstream HTTP status and body.

### Conversation and memory events

- summary job start and finish
- user memory refresh success or failure
- model selection and downloads

## Operational notes

- JSON lines keep the log stream simple for small self-hosted boxes.
- Mounted logs avoid losing history on container restarts.
- If prompts or responses may contain sensitive data, place log retention and access controls in front of the shared logs volume.
- For production, add log rotation on the host or via a sidecar if the volume is long-lived.
