-- Remove the reverse-tunnel extension registry.
--
-- The plugin model has no equivalent: Core starts plugins itself, so there is
-- no connection to authenticate and no secret to issue. A plugin's identity is
-- the parent/child relationship, and its approval is an operator installing
-- the package.
--
-- Nothing is migrated across. The two models are different runtime shapes —
-- a remote process dialling in versus a local subprocess Core forks — so there
-- is no executable to carry over. An operator reinstalls each extension as a
-- plugin package.

DROP TABLE IF EXISTS extension_secrets;
DROP TABLE IF EXISTS extensions;
DROP TABLE IF EXISTS extension_versions;
