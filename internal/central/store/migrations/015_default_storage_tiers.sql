-- Default storage tiers: seeded on first run, can be edited or deleted.

INSERT INTO storage_tiers (id, name, description) VALUES
    ('b0000000-0000-0000-0000-000000000001', 'default',     'General-purpose storage; used when no specific tier is required'),
    ('b0000000-0000-0000-0000-000000000002', 'replicated',  'Storage backed by replication for higher durability'),
    ('b0000000-0000-0000-0000-000000000003', 'snapshotted', 'Storage with snapshot support for fast cloning and recovery')
ON CONFLICT (name) DO NOTHING;
