-- Durable queue for plugins.
--
-- Topics are namespaced per plugin (owner_key). A plugin publishing to "jobs"
-- and another publishing to "jobs" are using two different queues, which
-- matches how collections are isolated in the document store: without it, one
-- plugin could consume another's messages simply by guessing a topic name.
-- Cross-plugin messaging goes through the event bus, which is explicit about
-- crossing that boundary.
CREATE TABLE IF NOT EXISTS plugin_queue (
    id              BIGSERIAL PRIMARY KEY,
    owner_key       VARCHAR(100) NOT NULL,
    topic           VARCHAR(200) NOT NULL,
    payload         BYTEA        NOT NULL,

    -- pending -> processing -> done, or -> dead once attempts run out.
    status          VARCHAR(20)  NOT NULL DEFAULT 'pending',

    priority        INT          NOT NULL DEFAULT 0,
    attempts        INT          NOT NULL DEFAULT 0,
    max_attempts    INT          NOT NULL DEFAULT 5,

    -- available_at supports both delayed messages and retry backoff.
    available_at    TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP,

    -- locked_until is the visibility timeout. A consumer that dies without
    -- acknowledging leaves the row locked only until this passes, after which
    -- the message becomes deliverable again rather than being lost.
    locked_until    TIMESTAMPTZ,

    dedup_key       VARCHAR(200),
    last_error      TEXT,

    -- parent_trace_id carries the trace of the request that enqueued the
    -- message, so asynchronous work stays attributable to whatever triggered
    -- it across the boundary where the original request has already finished.
    parent_trace_id VARCHAR(64),

    created_at      TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- The claim query filters on exactly these columns and orders by priority then
-- id, so this index is what keeps polling cheap as the done rows accumulate.
CREATE INDEX IF NOT EXISTS idx_plugin_queue_claim
    ON plugin_queue (owner_key, topic, priority DESC, id)
    WHERE status = 'pending';

-- Deduplication only applies to messages still in flight: once a message is
-- done, the same dedup key may legitimately be used again for the next one.
CREATE UNIQUE INDEX IF NOT EXISTS idx_plugin_queue_dedup
    ON plugin_queue (owner_key, topic, dedup_key)
    WHERE dedup_key IS NOT NULL AND status IN ('pending', 'processing');

-- Supports reaping expired visibility timeouts.
CREATE INDEX IF NOT EXISTS idx_plugin_queue_locked
    ON plugin_queue (locked_until)
    WHERE status = 'processing';
