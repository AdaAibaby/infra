// Package management holds the state changes the control-plane management
// interface applies. They live outside the handlers so they are reachable
// without gin: what these operations get wrong is never the HTTP.
//
// Each one owns the cache evictions its own write invalidates, and reports
// failures as sentinel errors rather than database ones, so the routes above
// map outcomes to status codes without knowing what backs them.
package management

import (
	"go.opentelemetry.io/otel/metric"

	sharedauth "github.com/e2b-dev/infra/packages/auth/pkg/auth"
	sqlcdb "github.com/e2b-dev/infra/packages/db/client"
)

type Service struct {
	db       *sqlcdb.Client
	cache    sharedauth.Service
	applyLag applyLag
}

func NewService(db *sqlcdb.Client, cache sharedauth.Service, meterProvider metric.MeterProvider) *Service {
	return &Service{db: db, cache: cache, applyLag: newApplyLag(meterProvider)}
}
