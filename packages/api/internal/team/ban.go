package team

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	template_manager "github.com/e2b-dev/infra/packages/api/internal/template-manager"
	sqlcdb "github.com/e2b-dev/infra/packages/db/client"
	dashboardqueries "github.com/e2b-dev/infra/packages/db/pkg/dashboard/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// banTeamsConcurrency bounds how many teams BanTeams bans and contains at once.
// Each team's kills and cancellations already run ten at a time, so this caps a
// bulk ban at fifty of each in flight.
const banTeamsConcurrency = 5

var ErrTeamNotFound = errors.New("team not found")

type teamCache interface {
	InvalidateTeamCache(ctx context.Context, teamID uuid.UUID) error
}

type sandboxKiller interface {
	KillTeamSandboxes(ctx context.Context, teamID uuid.UUID, reason sandbox.KillReason) (killed, failed int, err error)
}

type buildCanceller interface {
	CancelTeamBuilds(ctx context.Context, teamID uuid.UUID, reason string) (cancelled, failed int, err error)
}

type Service struct {
	db                *sqlcdb.Client
	cache             teamCache
	sandboxes         sandboxKiller
	builds            buildCanceller
	containmentBudget time.Duration
}

// NewService bounds each request's containment by containmentBudget.
func NewService(db *sqlcdb.Client, cache teamCache, sandboxes sandboxKiller, builds buildCanceller, containmentBudget time.Duration) *Service {
	return &Service{db: db, cache: cache, sandboxes: sandboxes, builds: builds, containmentBudget: containmentBudget}
}

// Containment is what a ban did to the team's running workloads. A step that
// failed or ran out of the budget carries its error beside what it counted.
type Containment struct {
	SandboxesKilled       int
	SandboxesFailedToKill int
	SandboxKillErr        error
	BuildsCancelled       int
	BuildsFailedToCancel  int
	BuildCancelErr        error
}

type BanOutcome struct {
	TeamID      uuid.UUID
	Containment Containment
	Err         error
}

// Ban marks the team banned, then kills its sandboxes and cancels its builds.
// The ban stands whatever containment does, and banning a banned team contains
// it again. Neither half stops when the caller goes away.
func (s *Service) Ban(ctx context.Context, teamID uuid.UUID) (Containment, error) {
	ctx = context.WithoutCancel(ctx)

	if err := s.setBanned(ctx, teamID, true); err != nil {
		return Containment{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, s.containmentBudget)
	defer cancel()

	return s.contain(ctx, teamID), nil
}

func (s *Service) Unban(ctx context.Context, teamID uuid.UUID) error {
	return s.setBanned(ctx, teamID, false)
}

// BanTeams stores every team's ban before containing any of them, so a slow
// containment cannot keep a later team unbanned, and contains the banned teams
// under one shared budget. One team's failure does not stop the others; the
// outcomes follow the order of teamIDs.
func (s *Service) BanTeams(ctx context.Context, teamIDs []uuid.UUID) []BanOutcome {
	ctx = context.WithoutCancel(ctx)
	outcomes := make([]BanOutcome, len(teamIDs))

	bans := errgroup.Group{}
	bans.SetLimit(banTeamsConcurrency)
	for i, teamID := range teamIDs {
		bans.Go(func() error {
			outcomes[i] = BanOutcome{TeamID: teamID, Err: s.setBanned(ctx, teamID, true)}

			return nil
		})
	}
	_ = bans.Wait()

	ctx, cancel := context.WithTimeout(ctx, s.containmentBudget)
	defer cancel()

	containments := errgroup.Group{}
	containments.SetLimit(banTeamsConcurrency)
	for i := range outcomes {
		if outcomes[i].Err != nil {
			continue
		}

		containments.Go(func() error {
			outcomes[i].Containment = s.contain(ctx, outcomes[i].TeamID)

			return nil
		})
	}
	_ = containments.Wait()

	return outcomes
}

func (s *Service) setBanned(ctx context.Context, teamID uuid.UUID, banned bool) error {
	rows, err := s.db.Dashboard.SetTeamBanned(ctx, dashboardqueries.SetTeamBannedParams{IsBanned: banned, TeamID: teamID})
	if err != nil {
		return fmt.Errorf("set team banned: %w", err)
	}
	if rows == 0 {
		return ErrTeamNotFound
	}

	// API keys authenticate against the cached team, so the change takes
	// effect only once the cache drops it.
	if err := s.cache.InvalidateTeamCache(ctx, teamID); err != nil {
		return fmt.Errorf("invalidate team cache: %w", err)
	}

	return nil
}

// contain kills the team's sandboxes and cancels its builds side by side, so a
// kill that uses up the budget cannot keep the builds from being cancelled. It
// reports a step the budget cut short as failed: the kill and cancel count the
// items they could not finish, but only return an error for a listing that
// failed.
func (s *Service) contain(ctx context.Context, teamID uuid.UUID) Containment {
	var containment Containment
	var wg sync.WaitGroup

	wg.Go(func() {
		killed, failed, err := s.sandboxes.KillTeamSandboxes(ctx, teamID, sandbox.KillReasonAdmin)
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			logger.L().Error(ctx, "Failed to kill banned team's sandboxes",
				logger.WithTeamID(teamID.String()), zap.Error(err))
		}

		containment.SandboxesKilled, containment.SandboxesFailedToKill, containment.SandboxKillErr = killed, failed, err
	})

	wg.Go(func() {
		cancelled, failed, err := s.builds.CancelTeamBuilds(ctx, teamID, template_manager.CancelledByAdminReason)
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			logger.L().Error(ctx, "Failed to cancel banned team's builds",
				logger.WithTeamID(teamID.String()), zap.Error(err))
		}

		containment.BuildsCancelled, containment.BuildsFailedToCancel, containment.BuildCancelErr = cancelled, failed, err
	})

	wg.Wait()

	return containment
}
