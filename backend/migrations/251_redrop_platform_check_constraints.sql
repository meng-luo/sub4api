-- Local migration 247 restores platform CHECK constraints after upstream 246.
-- Keep platform membership managed by the application catalog on fresh and
-- existing installations without changing checksums of historical migrations.
ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;
