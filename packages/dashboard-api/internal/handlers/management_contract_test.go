package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/dashboard-api/internal/api"
)

func TestManagementClusterRegistrationContainsOnlyClusterDescriptorFields(t *testing.T) {
	t.Parallel()

	swagger, err := api.GetSwagger()
	if err != nil {
		t.Fatalf("loading Dashboard API contract: %v", err)
	}

	schema := swagger.Components.Schemas["ManagementClusterRegistrationRequest"]
	if schema == nil || schema.Value == nil {
		t.Fatal("ManagementClusterRegistrationRequest schema is missing")
	}
	want := map[string]struct{}{
		"name": {}, "endpoint": {}, "endpoint_tls": {}, "token": {},
		"sandbox_proxy_domain": {}, "auth_org_id": {},
	}
	for name := range schema.Value.Properties {
		if _, ok := want[name]; !ok {
			t.Fatalf("management cluster registration exposes unexpected field %q", name)
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("management cluster registration is missing fields: %v", want)
	}
}

func TestEveryManagementRouteReachesItsHandler(t *testing.T) {
	t.Parallel()

	projectID := uuid.New().String()
	clusterID := uuid.New().String()
	project := "/v1/management/projects/" + projectID
	cluster := "/v1/management/clusters/" + clusterID
	projectCluster := project + "/cluster/" + clusterID

	for _, tt := range []struct {
		operation string
		method    string
		path      string
		body      string
	}{
		{"registerCluster", http.MethodPut, cluster, `{"name":"a","endpoint":"a:443","endpoint_tls":true,"token":"token"}`},
		{"deleteCluster", http.MethodDelete, cluster, ""},
		{"requestClusterDeletion", http.MethodPut, cluster + "/deletion", ""},
		{"listClusterProjects", http.MethodGet, cluster + "/projects", ""},
		{"assignProjectCluster", http.MethodPut, projectCluster, ""},
		{"detachProjectCluster", http.MethodDelete, projectCluster, ""},
	} {
		t.Run(tt.operation, func(t *testing.T) {
			t.Parallel()

			reached := make(chan string, 1)
			router := gin.New()
			api.RegisterHandlers(router, &routeRecorder{reached: reached})

			recorder := httptest.NewRecorder()
			request := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, strings.NewReader(tt.body))
			request.Header.Set("Content-Type", "application/json")

			router.ServeHTTP(recorder, request)

			select {
			case got := <-reached:
				if got != tt.operation {
					t.Fatalf("request reached %q, want %q", got, tt.operation)
				}
			default:
				t.Fatalf("no handler was reached; status %d", recorder.Code)
			}
		})
	}
}

type routeRecorder struct {
	api.ServerInterface

	reached chan string
}

func (r *routeRecorder) report(c *gin.Context, operation string) {
	r.reached <- operation
	c.Status(http.StatusNoContent)
}

func (r *routeRecorder) ManagementRegisterCluster(c *gin.Context, _ api.ClusterID) {
	r.report(c, "registerCluster")
}

func (r *routeRecorder) ManagementDeleteCluster(c *gin.Context, _ api.ClusterID) {
	r.report(c, "deleteCluster")
}

func (r *routeRecorder) ManagementListClusterProjects(c *gin.Context, _ api.ClusterID) {
	r.report(c, "listClusterProjects")
}

func (r *routeRecorder) ManagementRequestClusterDeletion(c *gin.Context, _ api.ClusterID) {
	r.report(c, "requestClusterDeletion")
}

func (r *routeRecorder) ManagementAssignProjectCluster(c *gin.Context, _ api.ProjectID, _ api.ClusterID) {
	r.report(c, "assignProjectCluster")
}

func (r *routeRecorder) ManagementDetachProjectCluster(c *gin.Context, _ api.ProjectID, _ api.ClusterID) {
	r.report(c, "detachProjectCluster")
}
