-- Admin-managed settings, per plugin.
--
-- Until now a plugin's configuration lived only in memory, which meant an
-- operator could not set one at all: manifests could declare settings and Core
-- could push changes to running plugins, but there was nowhere for a value to
-- be written down and nothing survived a restart.
--
-- Values are text because that is what reaches the plugin. The manifest's
-- declared type drives the console's input and nothing else — a manifest does
-- not get to decide what an operator actually typed.
CREATE TABLE IF NOT EXISTS plugin_config (
    plugin_key  VARCHAR(100) NOT NULL,
    config_key  VARCHAR(200) NOT NULL,
    value       TEXT         NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (plugin_key, config_key)
);
