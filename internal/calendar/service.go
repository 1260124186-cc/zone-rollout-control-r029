// Package calendar manages the timezone-aware rolling calendar attached to each
// rollout plan. It registers permit windows and pause periods, scopes them to
// the whole plan or specific node regions, and answers rollout-time questions
// through the pure domain evaluator shared by plan activation and lease
// allocation.
//
// The package blank-imports time/tzdata so timezone and DST interpretation is
// identical even on hosts without a system zone database.
package calendar

import (
	"context"
	"sort"
	"strings"
	"time"

	_ "time/tzdata"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/store"
)

type Service struct {
	store *store.Store
	clock platform.Clock
	ids   platform.IDGenerator
}

func New(stateStore *store.Store, clock platform.Clock, ids platform.IDGenerator) *Service {
	return &Service{store: stateStore, clock: clock, ids: ids}
}

// Register adds a new permit window or pause period to a plan's calendar.
func (s *Service) Register(
	ctx context.Context,
	planID string,
	raw domain.WindowInput,
) (domain.WindowEntry, error) {
	input := domain.NormalizeWindowInput(raw)
	if err := domain.ValidateWindowInput(input); err != nil {
		return domain.WindowEntry{}, err
	}
	now := s.clock.Now().UTC()
	var entry domain.WindowEntry
	err := s.store.Write(ctx, func(state *domain.State) error {
		plan, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		entry = buildEntry(s.ids.New("win"), planID, input, now)
		state.Windows[entry.ID] = entry
		plan.Version++
		state.Plans[planID] = plan
		store.AddAudit(state, s.ids, "window", entry.ID, "registered", now, map[string]any{
			"plan_id":  planID,
			"mode":     string(entry.Mode),
			"schedule": string(entry.Schedule),
		})
		return nil
	})
	return entry, err
}

// Replace fully replaces the mutable definition of an existing window while
// preserving its identity. Approvals already captured reference the definition
// they were evaluated against and are never rewritten.
func (s *Service) Replace(
	ctx context.Context,
	planID string,
	windowID string,
	raw domain.WindowInput,
) (domain.WindowEntry, error) {
	input := domain.NormalizeWindowInput(raw)
	if err := domain.ValidateWindowInput(input); err != nil {
		return domain.WindowEntry{}, err
	}
	now := s.clock.Now().UTC()
	var entry domain.WindowEntry
	err := s.store.Write(ctx, func(state *domain.State) error {
		plan, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		existing, ok := state.Windows[windowID]
		if !ok || existing.PlanID != planID {
			return domain.NotFound("window", windowID)
		}
		if existing.Deleted {
			return domain.InvalidState("window has been deleted", map[string]any{
				"window_id": windowID,
			})
		}
		entry = buildEntry(windowID, planID, input, now)
		entry.Version = existing.Version + 1
		entry.CreatedAt = existing.CreatedAt
		state.Windows[windowID] = entry
		plan.Version++
		state.Plans[planID] = plan
		store.AddAudit(state, s.ids, "window", windowID, "replaced", now, map[string]any{
			"plan_id":        planID,
			"window_id":      windowID,
			"window_version": entry.Version,
		})
		return nil
	})
	return entry, err
}

// Delete marks a window deleted. Its identity stays reserved so past decisions
// remain resolvable; a plan whose entries are all deleted runs all hours again.
func (s *Service) Delete(ctx context.Context, planID string, windowID string) error {
	now := s.clock.Now().UTC()
	return s.store.Write(ctx, func(state *domain.State) error {
		plan, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		entry, ok := state.Windows[windowID]
		if !ok || entry.PlanID != planID {
			return domain.NotFound("window", windowID)
		}
		if entry.Deleted {
			return nil
		}
		entry.Deleted = true
		entry.UpdatedAt = now
		entry.Version++
		state.Windows[windowID] = entry
		plan.Version++
		state.Plans[planID] = plan
		store.AddAudit(state, s.ids, "window", windowID, "deleted", now, map[string]any{
			"plan_id": planID,
		})
		return nil
	})
}

// Get returns a single window definition.
func (s *Service) Get(ctx context.Context, planID string, windowID string) (domain.WindowEntry, error) {
	var entry domain.WindowEntry
	err := s.store.Read(ctx, func(state *domain.State) error {
		found, ok := state.Windows[windowID]
		if !ok || found.PlanID != planID {
			return domain.NotFound("window", windowID)
		}
		entry = found
		return nil
	})
	return entry, err
}

// ListForPlan returns the active (non-deleted) windows of a plan sorted by ID.
func (s *Service) ListForPlan(ctx context.Context, planID string) ([]domain.WindowEntry, error) {
	entries := []domain.WindowEntry{}
	err := s.store.Read(ctx, func(state *domain.State) error {
		if _, ok := state.Plans[planID]; !ok {
			return domain.NotFound("plan", planID)
		}
		for _, entry := range state.Windows {
			if entry.PlanID == planID && !entry.Deleted {
				entries = append(entries, entry)
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
		return nil
	})
	return entries, err
}

// Evaluate answers a hypothetical or current rollout question without changing
// state. Region controls the scope: empty evaluates plan-wide activation; any
// value evaluates lease allocation for that region. When at is zero the
// service clock is used, which is the exact same interpretation the mutating
// endpoints apply.
func (s *Service) Evaluate(
	ctx context.Context,
	planID string,
	region string,
	at time.Time,
) (domain.WindowDecision, error) {
	region = strings.TrimSpace(strings.ToLower(region))
	if at.IsZero() {
		at = s.clock.Now().UTC()
	}
	var decision domain.WindowDecision
	err := s.store.Read(ctx, func(state *domain.State) error {
		plan, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		scope := domain.DecisionScopePlan
		if region != "" {
			scope = domain.DecisionScopeRegion
		}
		decision = domain.EvaluateCalendar(state, planID, plan.Zone, scope, region, at.UTC())
		return nil
	})
	return decision, err
}

func buildEntry(
	id string,
	planID string,
	input domain.NormalizedWindowInput,
	now time.Time,
) domain.WindowEntry {
	entry := domain.WindowEntry{
		ID:        id,
		PlanID:    planID,
		Mode:      input.Mode,
		Schedule:  input.Schedule,
		Timezone:  input.Timezone,
		StartTime: input.StartTime,
		EndTime:   input.EndTime,
		Regions:   append([]string(nil), input.Regions...),
		Note:      input.Note,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if input.Schedule == domain.WindowOnce {
		entry.StartsAt = input.StartsAt.UTC()
		entry.EndsAt = input.EndsAt.UTC()
	}
	if input.Schedule == domain.WindowWeekly {
		entry.Weekdays = append([]time.Weekday(nil), input.Weekdays...)
		sort.Slice(entry.Weekdays, func(i, j int) bool { return entry.Weekdays[i] < entry.Weekdays[j] })
	}
	return entry
}
