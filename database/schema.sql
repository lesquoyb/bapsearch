PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS conversations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    rewritten_query TEXT NOT NULL DEFAULT '',
    rewrite_status TEXT NOT NULL DEFAULT 'pending',
    rewrite_detail TEXT NOT NULL DEFAULT '',
    answer_status TEXT NOT NULL DEFAULT 'pending',
    answer_detail TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation_id INTEGER NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('user', 'system', 'assistant')),
    content TEXT NOT NULL,
    reasoning TEXT NOT NULL DEFAULT '',
    timestamp DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (conversation_id) REFERENCES conversations(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS search_results (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation_id INTEGER NOT NULL,
    url TEXT NOT NULL,
    title TEXT NOT NULL,
    snippet TEXT NOT NULL,
    query_variant TEXT NOT NULL DEFAULT 'original',
    rank INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (conversation_id) REFERENCES conversations(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS search_engine_statuses (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation_id INTEGER NOT NULL,
    engine TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'ok',
    detail TEXT NOT NULL DEFAULT '',
    result_count INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (conversation_id) REFERENCES conversations(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS summaries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation_id INTEGER NOT NULL,
    url TEXT NOT NULL,
    summary TEXT NOT NULL DEFAULT '',
    source_text TEXT NOT NULL DEFAULT '',
    embedding_json TEXT NOT NULL DEFAULT '',
    similarity_score REAL NOT NULL DEFAULT 0,
    rerank_position INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'pending',
    status_detail TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (conversation_id, url),
    FOREIGN KEY (conversation_id) REFERENCES conversations(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS user_memory (
    user_id TEXT PRIMARY KEY,
    memory_summary TEXT NOT NULL,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_conversations_user_updated ON conversations(user_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_conversation_timestamp ON messages(conversation_id, timestamp ASC);
CREATE INDEX IF NOT EXISTS idx_search_results_conversation_rank ON search_results(conversation_id, rank ASC);
CREATE INDEX IF NOT EXISTS idx_search_engine_statuses_conversation ON search_engine_statuses(conversation_id, engine ASC);
CREATE INDEX IF NOT EXISTS idx_summaries_conversation_created ON summaries(conversation_id, created_at ASC);
