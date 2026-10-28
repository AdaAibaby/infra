package management

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/metric/noop"

	sharedauth "github.com/e2b-dev/infra/packages/auth/pkg/auth"
	authtypes "github.com/e2b-dev/infra/packages/auth/pkg/types"
	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
)

func newService(db *testutils.Database) (*Service, *recordingCache) {
	cache := &recordingCache{}

	return NewService(db.SqlcClient, cache, noop.NewMeterProvider()), cache
}

// recordingCache captures the evictions, which are the half of these operations
// with no trace in the database.
type recordingCache struct {
	noopAuthService

	teams []uuid.UUID
}

func (c *recordingCache) reset() {
	c.teams = nil
}

func (c *recordingCache) InvalidateTeamCache(_ context.Context, teamID uuid.UUID) error {
	c.teams = append(c.teams, teamID)

	return nil
}

// noopAuthService covers what these tests never reach, so recordingCache only
// implements the method it asserts on.
type noopAuthService struct{}

var _ sharedauth.Service = noopAuthService{}

func (noopAuthService) ValidateAPIKey(context.Context, *gin.Context, string) (*authtypes.Team, *sharedauth.APIError) {
	return nil, nil
}

func (noopAuthService) ValidateAuthProviderToken(context.Context, *gin.Context, string) (uuid.UUID, *sharedauth.APIError) {
	return uuid.Nil, nil
}

func (noopAuthService) ValidateAuthProviderTeam(context.Context, *gin.Context, string) (*authtypes.Team, *sharedauth.APIError) {
	return nil, nil
}

func (noopAuthService) GetTeamByID(context.Context, uuid.UUID) (*authtypes.Team, error) {
	return nil, nil
}

func (noopAuthService) InvalidateTeamMemberCache(context.Context, uuid.UUID, string) {}

func (noopAuthService) InvalidateAPIKeyCache(context.Context, string) {}

func (noopAuthService) InvalidateTeamCache(context.Context, uuid.UUID) error {
	return nil
}

func (noopAuthService) Close(context.Context) error {
	return nil
}
