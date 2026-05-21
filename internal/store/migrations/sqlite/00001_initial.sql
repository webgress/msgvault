-- +goose Up
-- +goose StatementBegin

-- msgvault unified schema (SQLite)
-- Supports: Gmail, Apple Messages, Google Messages, WhatsApp
--
-- The FTS5 virtual table is created separately by Store.InitSchema after
-- goose has applied this migration. FTS5 is an optional SQLite module; the
-- post-goose path swallows "no such module: fts5" on binaries built without
-- the fts5 build tag, which goose's transactional run cannot do.

CREATE TABLE IF NOT EXISTS sources (
    id INTEGER PRIMARY KEY,
    source_type TEXT NOT NULL,
    identifier TEXT NOT NULL,
    display_name TEXT,

    google_user_id TEXT UNIQUE,

    last_sync_at DATETIME,
    sync_cursor TEXT,
    sync_config JSON,
    oauth_app TEXT,

    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,

    UNIQUE(source_type, identifier)
);

CREATE TABLE IF NOT EXISTS participants (
    id INTEGER PRIMARY KEY,
    email_address TEXT,
    phone_number TEXT,
    display_name TEXT,
    domain TEXT,

    canonical_id TEXT,

    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS participant_identifiers (
    id INTEGER PRIMARY KEY,
    participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    identifier_type TEXT NOT NULL,
    identifier_value TEXT NOT NULL,
    display_value TEXT,

    is_primary BOOLEAN DEFAULT FALSE,

    UNIQUE(identifier_type, identifier_value)
);

CREATE TABLE IF NOT EXISTS conversations (
    id INTEGER PRIMARY KEY,
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,

    source_conversation_id TEXT,

    conversation_type TEXT NOT NULL,
    title TEXT,

    participant_count INTEGER DEFAULT 0,
    message_count INTEGER DEFAULT 0,
    unread_count INTEGER DEFAULT 0,
    last_message_at DATETIME,
    last_message_preview TEXT,

    metadata JSON,

    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,

    UNIQUE(source_id, source_conversation_id)
);

CREATE TABLE IF NOT EXISTS conversation_participants (
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    role TEXT DEFAULT 'member',
    joined_at DATETIME,
    left_at DATETIME,

    PRIMARY KEY (conversation_id, participant_id)
);

CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY,
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,

    source_message_id TEXT,

    rfc822_message_id TEXT,

    message_type TEXT NOT NULL,

    sent_at DATETIME,
    received_at DATETIME,
    read_at DATETIME,
    delivered_at DATETIME,
    internal_date DATETIME,

    sender_id INTEGER REFERENCES participants(id),
    is_from_me BOOLEAN DEFAULT FALSE,

    subject TEXT,
    snippet TEXT,

    reply_to_message_id INTEGER REFERENCES messages(id),
    thread_position INTEGER,

    is_read BOOLEAN DEFAULT TRUE,
    is_delivered BOOLEAN,
    is_sent BOOLEAN DEFAULT TRUE,
    is_edited BOOLEAN DEFAULT FALSE,
    is_forwarded BOOLEAN DEFAULT FALSE,

    size_estimate INTEGER,
    has_attachments BOOLEAN DEFAULT FALSE,
    attachment_count INTEGER DEFAULT 0,

    deleted_at DATETIME,
    deleted_from_source_at DATETIME,
    delete_batch_id TEXT,

    archived_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    indexing_version INTEGER DEFAULT 1,

    metadata JSON,

    UNIQUE(source_id, source_message_id)
);

CREATE TABLE IF NOT EXISTS message_recipients (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    recipient_type TEXT NOT NULL,
    display_name TEXT,

    UNIQUE(message_id, participant_id, recipient_type)
);

CREATE TABLE IF NOT EXISTS reactions (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,

    reaction_type TEXT NOT NULL,
    reaction_value TEXT NOT NULL,

    created_at DATETIME,
    removed_at DATETIME,

    UNIQUE(message_id, participant_id, reaction_type, reaction_value)
);

CREATE TABLE IF NOT EXISTS attachments (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,

    filename TEXT,
    mime_type TEXT,
    size INTEGER,

    content_hash TEXT,
    storage_path TEXT NOT NULL,

    media_type TEXT,
    width INTEGER,
    height INTEGER,
    duration_ms INTEGER,

    thumbnail_hash TEXT,
    thumbnail_path TEXT,

    source_attachment_id TEXT,
    attachment_metadata JSON,

    encryption_version INTEGER DEFAULT 0,

    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS labels (
    id INTEGER PRIMARY KEY,
    source_id INTEGER REFERENCES sources(id) ON DELETE CASCADE,

    source_label_id TEXT,
    name TEXT NOT NULL,
    label_type TEXT,
    color TEXT,

    UNIQUE(source_id, name)
);

CREATE TABLE IF NOT EXISTS message_labels (
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    label_id INTEGER NOT NULL REFERENCES labels(id) ON DELETE CASCADE,

    PRIMARY KEY (message_id, label_id)
);

CREATE TABLE IF NOT EXISTS message_bodies (
    message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    body_text TEXT,
    body_html TEXT
);

CREATE TABLE IF NOT EXISTS message_raw (
    message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,

    raw_data BLOB NOT NULL,
    raw_format TEXT NOT NULL,

    compression TEXT DEFAULT 'zlib',
    encryption_version INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS sync_runs (
    id INTEGER PRIMARY KEY,
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,

    started_at DATETIME NOT NULL,
    completed_at DATETIME,
    status TEXT DEFAULT 'running',

    messages_processed INTEGER DEFAULT 0,
    messages_added INTEGER DEFAULT 0,
    messages_updated INTEGER DEFAULT 0,
    errors_count INTEGER DEFAULT 0,

    error_message TEXT,
    cursor_before TEXT,
    cursor_after TEXT
);

CREATE TABLE IF NOT EXISTS sync_checkpoints (
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    checkpoint_type TEXT NOT NULL,
    checkpoint_value TEXT NOT NULL,

    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (source_id, checkpoint_type)
);

CREATE INDEX IF NOT EXISTS idx_sources_type ON sources(source_type);

CREATE UNIQUE INDEX IF NOT EXISTS idx_participants_email ON participants(email_address)
    WHERE email_address IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_participants_phone ON participants(phone_number)
    WHERE phone_number IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_participants_canonical ON participants(canonical_id)
    WHERE canonical_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_participant_identifiers_value ON participant_identifiers(identifier_value);
CREATE INDEX IF NOT EXISTS idx_participant_identifiers_participant ON participant_identifiers(participant_id);

CREATE INDEX IF NOT EXISTS idx_conversations_source ON conversations(source_id);
CREATE INDEX IF NOT EXISTS idx_conversations_last_message ON conversations(last_message_at DESC);
CREATE INDEX IF NOT EXISTS idx_conversations_type ON conversations(conversation_type);

CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id, sent_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_source ON messages(source_id);
CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender_id);
CREATE INDEX IF NOT EXISTS idx_messages_sent_at ON messages(sent_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_type ON messages(message_type);
CREATE INDEX IF NOT EXISTS idx_messages_deleted ON messages(source_id, deleted_from_source_at);
CREATE INDEX IF NOT EXISTS idx_messages_source_message_id ON messages(source_message_id);

CREATE INDEX IF NOT EXISTS idx_message_recipients_message ON message_recipients(message_id);
CREATE INDEX IF NOT EXISTS idx_message_recipients_participant ON message_recipients(participant_id, recipient_type);

CREATE INDEX IF NOT EXISTS idx_reactions_message ON reactions(message_id);

CREATE INDEX IF NOT EXISTS idx_attachments_message ON attachments(message_id);
CREATE INDEX IF NOT EXISTS idx_attachments_hash ON attachments(content_hash);
CREATE INDEX IF NOT EXISTS idx_attachments_storage_path ON attachments(storage_path);

CREATE INDEX IF NOT EXISTS idx_labels_source ON labels(source_id);
CREATE INDEX IF NOT EXISTS idx_message_labels_label ON message_labels(label_id);

CREATE INDEX IF NOT EXISTS idx_sync_runs_source ON sync_runs(source_id, started_at DESC);

CREATE TABLE IF NOT EXISTS collections (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL UNIQUE,
    description TEXT    NOT NULL DEFAULT '',
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS collection_sources (
    collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    source_id     INTEGER NOT NULL REFERENCES sources(id)     ON DELETE CASCADE,
    PRIMARY KEY (collection_id, source_id)
);

CREATE INDEX IF NOT EXISTS idx_collection_sources_source_id
    ON collection_sources(source_id);

CREATE TABLE IF NOT EXISTS account_identities (
    source_id    INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    address      TEXT NOT NULL,
    source_signal TEXT NOT NULL DEFAULT '',
    confirmed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (source_id, address)
);

CREATE INDEX IF NOT EXISTS idx_account_identities_address
    ON account_identities(address);

CREATE TABLE IF NOT EXISTS applied_migrations (
    name        TEXT PRIMARY KEY,
    applied_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS applied_migrations;
DROP TABLE IF EXISTS account_identities;
DROP TABLE IF EXISTS collection_sources;
DROP TABLE IF EXISTS collections;
DROP TABLE IF EXISTS sync_checkpoints;
DROP TABLE IF EXISTS sync_runs;
DROP TABLE IF EXISTS message_raw;
DROP TABLE IF EXISTS message_bodies;
DROP TABLE IF EXISTS message_labels;
DROP TABLE IF EXISTS labels;
DROP TABLE IF EXISTS attachments;
DROP TABLE IF EXISTS reactions;
DROP TABLE IF EXISTS message_recipients;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS conversation_participants;
DROP TABLE IF EXISTS conversations;
DROP TABLE IF EXISTS participant_identifiers;
DROP TABLE IF EXISTS participants;
DROP TABLE IF EXISTS sources;
-- +goose StatementEnd
