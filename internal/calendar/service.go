package calendar

import (
	"context"
	"sort"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/schedule"
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

func (s *Service) Create(ctx context.Context, input domain.CalendarInput) (domain.Calendar, error) {
	input = domain.NormalizeCalendarInput(input)
	if err := domain.ValidateCalendarInput(input); err != nil {
		return domain.Calendar{}, err
	}
	now := s.clock.Now().UTC()
	calendar := domain.Calendar{
		ID:           s.ids.New("cal"),
		Name:         input.Name,
		Timezone:     input.Timezone,
		ScopeZones:   append([]string(nil), input.ScopeZones...),
		ScopeRegions: append([]string(nil), input.ScopeRegions...),
		Version:      1,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	err := s.store.Write(ctx, func(state *domain.State) error {
		state.Calendars[calendar.ID] = calendar
		store.AddAudit(state, s.ids, "calendar", calendar.ID, "created", now, map[string]any{
			"name": calendar.Name,
		})
		return nil
	})
	return calendar, err
}

func (s *Service) Get(ctx context.Context, calendarID string) (domain.CalendarDetail, error) {
	var detail domain.CalendarDetail
	err := s.store.Read(ctx, func(state *domain.State) error {
		calendar, ok := state.Calendars[calendarID]
		if !ok {
			return domain.NotFound("calendar", calendarID)
		}
		detail.Calendar = calendar
		for _, entry := range state.Windows {
			if entry.CalendarID == calendarID && entry.ArchivedAt == nil {
				detail.Windows = append(detail.Windows, entry)
			}
		}
		sort.Slice(detail.Windows, func(i, j int) bool {
			return detail.Windows[i].ID < detail.Windows[j].ID
		})
		return nil
	})
	return detail, err
}

func (s *Service) List(
	ctx context.Context,
	includeArchived bool,
	limit int,
	cursor string,
) (domain.CalendarPage, error) {
	if limit < 1 || limit > 100 {
		return domain.CalendarPage{}, domain.Validation("limit must be between 1 and 100", map[string]any{
			"limit": limit,
		})
	}
	after, err := platform.DecodeCursor(cursor)
	if err != nil {
		return domain.CalendarPage{}, domain.Validation("cursor is invalid", nil)
	}
	var page domain.CalendarPage
	err = s.store.Read(ctx, func(state *domain.State) error {
		items := make([]domain.Calendar, 0, len(state.Calendars))
		for _, calendar := range state.Calendars {
			if !includeArchived && calendar.ArchivedAt != nil {
				continue
			}
			if after != "" && calendar.ID <= after {
				continue
			}
			items = append(items, calendar)
		}
		sort.Slice(items, func(i, j int) bool {
			return items[i].ID < items[j].ID
		})
		page.Meta.Limit = limit
		if len(items) > limit {
			page.Items = append(page.Items, items[:limit]...)
			page.Meta.NextCursor = platform.EncodeCursor(items[limit-1].ID)
			return nil
		}
		page.Items = append(page.Items, items...)
		return nil
	})
	return page, err
}

// Archive removes a calendar (and keeps its history) by tombstoning it and
// all live entries. Archived calendars no longer affect admission, but
// frozen admission snapshots still reference them.
func (s *Service) Archive(ctx context.Context, calendarID string) error {
	now := s.clock.Now().UTC()
	return s.store.Write(ctx, func(state *domain.State) error {
		calendar, ok := state.Calendars[calendarID]
		if !ok {
			return domain.NotFound("calendar", calendarID)
		}
		if calendar.ArchivedAt != nil {
			return domain.InvalidState("calendar is already archived", map[string]any{
				"calendar_id": calendarID,
			})
		}
		calendar.ArchivedAt = &now
		calendar.Version++
		calendar.UpdatedAt = now
		state.Calendars[calendarID] = calendar
		archivedEntries := 0
		for entryID, entry := range state.Windows {
			if entry.CalendarID != calendarID || entry.ArchivedAt != nil {
				continue
			}
			entry.ArchivedAt = &now
			entry.Version++
			entry.UpdatedAt = now
			state.Windows[entryID] = entry
			archivedEntries++
		}
		store.AddAudit(state, s.ids, "calendar", calendarID, "archived", now, map[string]any{
			"window_count": archivedEntries,
		})
		return nil
	})
}

func (s *Service) AddWindow(
	ctx context.Context,
	calendarID string,
	input domain.WindowInput,
) (domain.MaintenanceWindow, error) {
	input = domain.NormalizeWindowInput(input)
	if err := domain.ValidateWindowInput(input); err != nil {
		return domain.MaintenanceWindow{}, err
	}
	now := s.clock.Now().UTC()
	entry, err := buildWindow(s.ids.New("win"), calendarID, input, now)
	if err != nil {
		return domain.MaintenanceWindow{}, err
	}
	err = s.store.Write(ctx, func(state *domain.State) error {
		calendar, ok := state.Calendars[calendarID]
		if !ok {
			return domain.NotFound("calendar", calendarID)
		}
		if calendar.ArchivedAt != nil {
			return domain.InvalidState("cannot add windows to an archived calendar", map[string]any{
				"calendar_id": calendarID,
			})
		}
		state.Windows[entry.ID] = entry
		calendar.Version++
		calendar.UpdatedAt = now
		state.Calendars[calendarID] = calendar
		store.AddAudit(state, s.ids, "maintenance_window", entry.ID, "registered", now, map[string]any{
			"calendar_id": calendarID,
			"kind":        entry.Kind,
			"schedule":    entry.Schedule,
		})
		return nil
	})
	return entry, err
}

// ArchiveWindow tombstones a single entry. It never mutates or deletes an
// entry in place, so admission already granted remains interpretable.
func (s *Service) ArchiveWindow(ctx context.Context, calendarID, windowID string) error {
	now := s.clock.Now().UTC()
	return s.store.Write(ctx, func(state *domain.State) error {
		calendar, ok := state.Calendars[calendarID]
		if !ok {
			return domain.NotFound("calendar", calendarID)
		}
		entry, ok := state.Windows[windowID]
		if !ok || entry.CalendarID != calendarID {
			return domain.NotFound("maintenance_window", windowID)
		}
		if entry.ArchivedAt != nil {
			return domain.InvalidState("maintenance window is already archived", map[string]any{
				"window_id": windowID,
			})
		}
		entry.ArchivedAt = &now
		entry.Version++
		entry.UpdatedAt = now
		state.Windows[windowID] = entry
		calendar.Version++
		calendar.UpdatedAt = now
		state.Calendars[calendarID] = calendar
		store.AddAudit(state, s.ids, "maintenance_window", windowID, "archived", now, map[string]any{
			"calendar_id": calendarID,
		})
		return nil
	})
}

// Resolve runs the shared admission evaluation without changing state. When
// at is the zero value the service clock is used, supporting "can I proceed
// now" probes over HTTP.
func (s *Service) Resolve(
	ctx context.Context,
	scope schedule.Scope,
	at time.Time,
) (domain.AdmissionDecision, error) {
	if at.IsZero() {
		at = s.clock.Now().UTC()
	}
	var decision domain.AdmissionDecision
	err := s.store.Read(ctx, func(state *domain.State) error {
		calendars := make([]domain.Calendar, 0, len(state.Calendars))
		for _, calendar := range state.Calendars {
			calendars = append(calendars, calendar)
		}
		windows := make([]domain.MaintenanceWindow, 0, len(state.Windows))
		for _, entry := range state.Windows {
			windows = append(windows, entry)
		}
		decision = schedule.Evaluate(calendars, windows, scope, at.UTC())
		return nil
	})
	return decision, err
}

func buildWindow(id, calendarID string, input domain.WindowInput, now time.Time) (domain.MaintenanceWindow, error) {
	entry := domain.MaintenanceWindow{
		ID:         id,
		CalendarID: calendarID,
		Name:       input.Name,
		Kind:       domain.CalendarEntryKind(input.Kind),
		Schedule:   domain.WindowSchedule(input.Schedule),
		Priority:   input.Priority,
		Version:    1,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	switch entry.Schedule {
	case domain.ScheduleOneTime:
		if input.OneTime == nil {
			return domain.MaintenanceWindow{}, domain.Validation("one_time range is required", nil)
		}
		entry.OneTime = &domain.OneTimeRange{
			StartAt: input.OneTime.StartAt.UTC(),
			EndAt:   input.OneTime.EndAt.UTC(),
		}
	case domain.ScheduleWeekly:
		if input.Weekly == nil {
			return domain.MaintenanceWindow{}, domain.Validation("weekly schedule is required", nil)
		}
		startMinute, ok := domain.ParseWeeklyStart(input.Weekly.Start)
		if !ok {
			return domain.MaintenanceWindow{}, domain.Validation("weekly start must be HH:MM", nil)
		}
		policy := domain.DSTGapPolicy(input.Weekly.GapPolicy)
		entry.Weekly = &domain.WeeklySchedule{
			Weekday:         time.Weekday(input.Weekly.Weekday),
			StartMinute:     startMinute,
			DurationMinutes: input.Weekly.DurationMinutes,
			GapPolicy:       policy,
		}
	}
	return entry, nil
}
