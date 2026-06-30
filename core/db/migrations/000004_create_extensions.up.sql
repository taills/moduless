-- extensions holds the admin-managed registry of extension identities and their
-- approval status. An extension is first seen as 'pending' when it dials Core
-- without a valid secret; an admin then approves or rejects it. Only 'approved'
-- extensions are routed.
CREATE TABLE IF NOT EXISTS extensions (
    key          VARCHAR(100) PRIMARY KEY,
    display_name VARCHAR(255) NOT NULL DEFAULT '',
    version      VARCHAR(50)  NOT NULL DEFAULT '',
    menu_icon    VARCHAR(100) NOT NULL DEFAULT '',
    menu_path    VARCHAR(255) NOT NULL DEFAULT '',
    status       VARCHAR(20)  NOT NULL DEFAULT 'pending', -- pending | approved | rejected
    created_at   TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    approved_at  TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

-- extension_secrets is a one-to-many child of extensions: a single key may have
-- many valid secrets, one per running instance, so multi-replica deployments can
-- each carry an independently revocable credential.
CREATE TABLE IF NOT EXISTS extension_secrets (
    id            BIGSERIAL PRIMARY KEY,
    extension_key VARCHAR(100) NOT NULL REFERENCES extensions(key) ON DELETE CASCADE,
    secret_hash   VARCHAR(255) NOT NULL,
    label         VARCHAR(100) NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    last_used_at  TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_ext_secrets_active
    ON extension_secrets (extension_key) WHERE revoked_at IS NULL;
