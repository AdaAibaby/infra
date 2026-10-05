package tests

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/db/queries"
)

// Template builds and snapshot builds set env_builds.env_id and team_id when
// they are inserted, so the build is complete without the backfill triggers
// on env_build_assignments. The triggers are disabled here to prove it.
func TestBuildInsertsSetEnvAndTeamWithoutTriggers(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	ctx := t.Context()
	require.NoError(t, db.SqlcClient.TestsRawSQL(ctx, `ALTER TABLE public.env_build_assignments DISABLE TRIGGER USER`))

	teamID := testutils.CreateTestTeam(t, db)
	templateID := testutils.CreateTestTemplate(t, db, teamID)
	read := func(t *testing.T, buildID uuid.UUID) (*string, *uuid.UUID) {
		t.Helper()
		var envID *string
		var buildTeamID *uuid.UUID
		require.NoError(t, db.SqlcClient.TestsRawSQLQuery(ctx, `SELECT env_id, team_id FROM public.env_builds WHERE id = $1`,
			func(rows pgx.Rows) error {
				require.True(t, rows.Next())

				return rows.Scan(&envID, &buildTeamID)
			}, buildID))

		return envID, buildTeamID
	}

	buildID := uuid.New()
	require.NoError(t, db.SqlcClient.CreateTemplateBuild(ctx, queries.CreateTemplateBuildParams{
		BuildID:            buildID,
		TemplateID:         templateID,
		Status:             types.BuildStatusWaiting,
		RamMb:              512,
		Vcpu:               1,
		KernelVersion:      "6.1.0",
		FirecrackerVersion: "1.4.0",
		FreeDiskSizeMb:     512,
	}))
	envID, buildTeamID := read(t, buildID)
	require.Equal(t, &templateID, envID)
	require.Equal(t, &teamID, buildTeamID)

	// The first pause creates the snapshot env; a second pause of the same
	// sandbox adds a build to the existing one.
	sandboxID := "sbx-" + uuid.NewString()
	for range 2 {
		snapshot := testutils.UpsertTestSnapshot(t, ctx, db, "snap-"+uuid.NewString(), sandboxID, teamID, templateID)
		envID, buildTeamID := read(t, snapshot.BuildID)
		require.Equal(t, &snapshot.TemplateID, envID)
		require.Equal(t, &teamID, buildTeamID)
	}
}
