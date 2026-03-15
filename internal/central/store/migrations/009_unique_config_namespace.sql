ALTER TABLE cluster_configs DROP CONSTRAINT IF EXISTS cluster_configs_name_satellite_id_key;
ALTER TABLE cluster_configs ADD CONSTRAINT cluster_configs_name_namespace_satellite_id_key UNIQUE(name, namespace, satellite_id);
