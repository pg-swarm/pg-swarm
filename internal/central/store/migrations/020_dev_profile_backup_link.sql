-- Link the dev cluster profile to the dev-physical-every-minute backup profile.

INSERT INTO profile_backup_profiles (profile_id, backup_profile_id) VALUES (
    'c0000000-0000-0000-0000-000000000001',
    'a0000000-0000-0000-0000-000000000003'
) ON CONFLICT DO NOTHING;
