package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStaticAndParamSiblingsCoexist guards against gin's router rejecting or
// misrouting the sibling pair POST /admin/users/bootstrap and
// POST /admin/users/:userId/bootstrap. The pair was flagged as a potential
// httprouter conflict; this verifies gin handles it correctly.
func TestStaticAndParamSiblingsCoexist(t *testing.T) {
	t.Parallel()

	r := gin.New()

	require.NotPanics(t, func() {
		r.POST("/admin/users/bootstrap", func(c *gin.Context) {
			c.String(http.StatusOK, "static")
		})
		r.POST("/admin/users/:userId/bootstrap", func(c *gin.Context) {
			c.String(http.StatusOK, "param:"+c.Param("userId"))
		})
	}, "gin must accept sibling static and parameter segments at the same level")

	cases := []struct {
		path string
		want string
	}{
		{"/admin/users/bootstrap", "static"},
		{"/admin/users/abc-123/bootstrap", "param:abc-123"},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(
				t.Context(),
				http.MethodPost,
				tc.path,
				nil,
			)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, tc.want, rec.Body.String())
		})
	}
}

func TestSandboxRecordDoesNotExposeApiKeyAuth(t *testing.T) {
	t.Parallel()

	swagger, err := GetSwagger()
	require.NoError(t, err)

	operation := operationForRoute(t, swagger, http.MethodGet, "/sandboxes/{sandboxID}/record")
	require.False(t, securityIncludesScheme(operation.Security, "ApiKeyAuth"), "sandbox recording must stay out of API-key auth")
}

func operationForRoute(t *testing.T, swagger *openapi3.T, method string, path string) *openapi3.Operation {
	t.Helper()

	pathItem := swagger.Paths.Find(path)
	require.NotNil(t, pathItem, "path %s must exist", path)

	operation := pathItem.GetOperation(method)
	require.NotNil(t, operation, "%s %s must exist", method, path)

	return operation
}

func securityIncludesScheme(security *openapi3.SecurityRequirements, scheme string) bool {
	if security == nil {
		return false
	}

	for _, requirement := range *security {
		if _, ok := requirement[scheme]; ok {
			return true
		}
	}

	return false
}
