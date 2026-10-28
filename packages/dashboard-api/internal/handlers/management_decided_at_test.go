package handlers

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/dashboard-api/internal/api"
	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
)

const sentDecidedAtJSON = `"2026-09-29T08:00:00.123456789Z"`

var sentDecidedAt = time.Date(2026, 9, 29, 8, 0, 0, 123456000, time.UTC)

func TestABlockDeliveryRecordsWhenItsRevisionWasDecided(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	teamID := testutils.CreateTestTeam(t, db)
	store, _ := newBlockStore(db)

	body := `{"revision": 2, "blocked": true, "decided_at": ` + sentDecidedAtJSON + `}`
	require.Equal(t, http.StatusNoContent, callApplyProjectBlock(t, store, teamID, body).Code)
	require.Equal(t, sentDecidedAt, *readLedgerDecidedAt(t, db, teamID),
		"stored to the microsecond Postgres keeps")
}

func TestABlockDeliveryWithoutDecidedAtStillApplies(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	teamID := testutils.CreateTestTeam(t, db)
	store, _ := newBlockStore(db)

	require.Equal(t, http.StatusNoContent, callApplyProjectBlock(t, store, teamID, `{"revision": 1, "blocked": true}`).Code)
	require.Nil(t, readLedgerDecidedAt(t, db, teamID))
}

func TestManagementContractAdmitsDecidedAt(t *testing.T) {
	t.Parallel()

	swagger, err := api.GetSwagger()
	require.NoError(t, err)
	schema := swagger.Components.Schemas["ManagementProjectBlockRequest"].Value
	property := schema.Properties["decided_at"]
	require.NotNil(t, property)
	require.Equal(t, "date-time", property.Value.Format)
	require.NotContains(t, schema.Required, "decided_at", "callers that do not send it must stay admitted")
}

func readLedgerDecidedAt(t *testing.T, db *testutils.Database, teamID uuid.UUID) *time.Time {
	t.Helper()

	var decidedAt *time.Time
	require.NoError(t, db.SqlcClient.TestsRawSQLQuery(t.Context(),
		"SELECT decided_at FROM projection.project_blocks WHERE project_id = $1",
		func(rows pgx.Rows) error {
			require.True(t, rows.Next(), "no ledger row")

			return rows.Scan(&decidedAt)
		}, teamID))
	if decidedAt != nil {
		utc := decidedAt.UTC()
		decidedAt = &utc
	}

	return decidedAt
}
