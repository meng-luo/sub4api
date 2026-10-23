-- Local releases applied TypeSafe as 247, followed by the constraint removal in
-- 251. Replaying upstream 241 on those databases can reject newer platforms or
-- restore constraints after their removal migrations have already been recorded.
-- Record its upstream checksum when the equivalent local migration was applied.
-- Fresh installations still execute upstream 241 normally.

INSERT INTO schema_migrations (filename, checksum)
SELECT '241_add_typesafe_platform.sql',
       'b6559525bf8d0b5d7c617e7415944f0d1fae8c408e9131ace84139795f66dcd2'
WHERE EXISTS (
    SELECT 1 FROM schema_migrations
    WHERE filename = '247_add_typesafe_platform.sql'
)
ON CONFLICT (filename) DO NOTHING;
