CREATE TABLE IF NOT EXISTS system_files (
    id VARCHAR(100) PRIMARY KEY,
    filename VARCHAR(255) NOT NULL,
    size BIGINT NOT NULL,
    mime_type VARCHAR(100) NOT NULL,
    storage_key VARCHAR(255) NOT NULL,
    uploader_id VARCHAR(100) NOT NULL,
    status VARCHAR(20) DEFAULT 'temporary',
    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS file_download_tokens (
    token VARCHAR(255) PRIMARY KEY,
    file_id VARCHAR(100) NOT NULL,
    user_id VARCHAR(100) NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
