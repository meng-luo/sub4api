//go:build integration

package repository

import (
	"context"
	"testing"

	dbmigrations "github.com/MACOS-DO/sub4api/migrations"
	"github.com/stretchr/testify/require"
)

const localTypeSafeCompatibilityMigration = "241_00_local_typesafe_compat.sql"

func TestLocalTypeSafeUpgradePreservesCatalogPlatforms(t *testing.T) {
	for _, platform := range []string{"anthropic", "cline", "command_code"} {
		t.Run(platform, func(t *testing.T) {
			ctx := context.Background()
			client := testEntClient(t)
			userID := mustCreateUserForQuota(t, client)
			t.Cleanup(func() {
				_, err := integrationDB.ExecContext(ctx, `DELETE FROM user_platform_quotas WHERE user_id = $1`, userID)
				require.NoError(t, err)
				_, err = integrationDB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
				require.NoError(t, err)
				_, err = integrationDB.ExecContext(ctx, `
INSERT INTO schema_migrations (filename, checksum) VALUES ($1, $2)
ON CONFLICT (filename) DO UPDATE SET checksum = EXCLUDED.checksum`,
					migration241Name, migration241UpstreamChecksum)
				require.NoError(t, err)
				content, err := dbmigrations.FS.ReadFile("251_redrop_platform_check_constraints.sql")
				require.NoError(t, err)
				_, err = integrationDB.ExecContext(ctx, string(content))
				require.NoError(t, err)
			})

			_, err := integrationDB.ExecContext(ctx, `
INSERT INTO user_platform_quotas (user_id, platform, daily_limit_usd)
VALUES ($1, $2, 1)`, userID, platform)
			require.NoError(t, err)

			// The local release already ran 247/251, but never had upstream 241.
			var applied int
			require.NoError(t, integrationDB.QueryRowContext(ctx, `
SELECT COUNT(*) FROM schema_migrations WHERE filename IN (
'247_add_typesafe_platform.sql', '251_redrop_platform_check_constraints.sql')`).Scan(&applied))
			require.Equal(t, 2, applied)
			_, err = integrationDB.ExecContext(ctx, `
DELETE FROM schema_migrations WHERE filename IN ($1, $2)`,
				migration241Name, localTypeSafeCompatibilityMigration)
			require.NoError(t, err)

			require.NoError(t, ApplyMigrations(ctx, integrationDB))
			require.False(t, platformConstraintExists(t, ctx, client, "user_platform_quotas", "user_platform_quotas_platform_check"))
			require.False(t, platformConstraintExists(t, ctx, client, "composite_model_routes", "composite_model_routes_target_platform_check"))
			checksum, exists := migration241Checksum(t, ctx, client)
			require.True(t, exists)
			require.Equal(t, migration241UpstreamChecksum, checksum)
			require.NoError(t, ApplyMigrations(ctx, integrationDB))
			var storedPlatform string
			require.NoError(t, integrationDB.QueryRowContext(ctx,
				`SELECT platform FROM user_platform_quotas WHERE user_id = $1`, userID).Scan(&storedPlatform))
			require.Equal(t, platform, storedPlatform)
		})
	}
}
