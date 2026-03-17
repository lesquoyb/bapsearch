# Logging Strategy

bap-search writes structured JSON logs to the mounted logs volume and stdout.

Primary log file:

- `/logs/backend.jsonl`

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

### LLM events

- serialized prompt content
- model response text
- inference errors

### Conversation and memory events

- summary job start and finish
- user memory refresh success or failure
- model selection and downloads

## Operational notes

- JSON lines keep the log stream simple for small self-hosted boxes.
- Mounted logs avoid losing history on container restarts.
- If prompts or responses may contain sensitive data, place log retention and access controls in front of the shared logs volume.
- For production, add log rotation on the host or via a sidecar if the volume is long-lived.
