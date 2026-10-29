package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/dashboard-api/internal/api"
	dashboardmiddleware "github.com/e2b-dev/infra/packages/dashboard-api/internal/middleware"
	dashboardqueries "github.com/e2b-dev/infra/packages/db/pkg/dashboard/queries"
	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
)

// A deleted project keeps its row, so a delivery that arrives after the
// deletion would still find it. The deleted-project middleware answers every
// write with 404 and leaves the project as the deletion left it.
func TestWritesToADeletedProjectAnswerNotFoundAndChangeNothing(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	clusterID, err := db.SqlcClient.Dashboard.CreateCluster(t.Context(), dashboardqueries.CreateClusterParams{
		Name: "deleted-project-cluster", Endpoint: "https://cluster.example.test", Token: "test-token",
	})
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		method string
		path   string
		body   string
	}{
		"cluster assignment": {http.MethodPut, "/v1/management/projects/%s/cluster/" + clusterID.String(), ""},
		"cluster detachment": {http.MethodDelete, "/v1/management/projects/%s/cluster/" + clusterID.String(), ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			router, auth := newDeletedProjectRouter(t, db)
			projectID := testutils.CreateTestTeam(t, db)
			markProjectDeleted(t, db, projectID)
			before := readDeletedProjectState(t, db, projectID)

			response := serveProjectRequest(t, router, tc.method, strings.Replace(tc.path, "%s", projectID.String(), 1), tc.body)

			require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
			require.JSONEq(t, `{"code":404,"message":"Project not found"}`, response.Body.String())
			require.Equal(t, before, readDeletedProjectState(t, db, projectID))
			require.Empty(t, auth.invalidated)
		})
	}
}

func TestALiveProjectPassesTheDeletedProjectCheck(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	clusterID, err := db.SqlcClient.Dashboard.CreateCluster(t.Context(), dashboardqueries.CreateClusterParams{
		Name: "live-project-cluster", Endpoint: "https://cluster.example.test", Token: "test-token",
	})
	require.NoError(t, err)
	router, _ := newDeletedProjectRouter(t, db)
	projectID := testutils.CreateTestTeam(t, db)

	response := serveProjectRequest(t, router, http.MethodPut,
		"/v1/management/projects/"+projectID.String()+"/cluster/"+clusterID.String(), "")

	require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
}

// newDeletedProjectRouter serves the handlers behind the deleted-project
// middleware, as the server does after authentication.
func newDeletedProjectRouter(t *testing.T, db *testutils.Database) (*gin.Engine, *recordingCacheAuthService) {
	t.Helper()

	auth := &recordingCacheAuthService{}
	store := &APIStore{db: db.SqlcClient, authService: auth}
	router := gin.New()
	router.Use(dashboardmiddleware.RefuseDeletedProject(db.SqlcClient.Dashboard))
	api.RegisterHandlers(router, store)

	return router, auth
}

func serveProjectRequest(t *testing.T, router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	return response
}

// markProjectDeleted leaves a project the way a deletion does: marked deleted,
// blocked, and released from its cluster.
func markProjectDeleted(t *testing.T, db *testutils.Database, projectID uuid.UUID) {
	t.Helper()

	require.NoError(t, db.SqlcClient.TestsRawSQL(t.Context(),
		`UPDATE public.teams SET deleted_at = now(), is_blocked = true, blocked_reason = 'Project deletion', cluster_id = NULL WHERE id = $1`,
		projectID))
}

// readDeletedProjectState reads the team row, which every guarded write
// changes.
func readDeletedProjectState(t *testing.T, db *testutils.Database, projectID uuid.UUID) string {
	t.Helper()

	var state string
	require.NoError(t, db.SqlcClient.TestsRawSQLQuery(t.Context(),
		`SELECT to_jsonb(t)::text FROM public.teams t WHERE t.id = $1`,
		func(rows pgx.Rows) error {
			require.True(t, rows.Next())

			return rows.Scan(&state)
		}, projectID))

	return state
}
