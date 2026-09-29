package schedule

import (
	"sort"
	"strings"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
)

// Scope describes the rollout action being admitted.
type Scope struct {
	Zone   string
	Region string
}

// calendarView is one calendar and the entries considered for it.
type calendarView struct {
	calendar domain.Calendar
	timezone string
	entries  []domain.MaintenanceWindow
}

// Evaluate decides whether a rollout action may proceed at instant.
//
// It is the single time interpretation shared by plan activation and node
// allocation. With no applicable calendar the result is the frozen form of
// "always open" (projects without a calendar remain deployable at any time).
// Otherwise every matching calendar contributes its windows; a suspension
// from any matching calendar vetoes the instant, and the admitting window is
// selected deterministically when windows overlap:
//
//  1. lower priority value wins (explicit operational ordering);
//  2. one-time windows win over weekly windows (specific wins over recurring);
//  3. the longer window wins;
//  4. the stable window id wins.
func Evaluate(calendars []domain.Calendar, windows []domain.MaintenanceWindow, scope Scope, instant time.Time) domain.AdmissionDecision {
	now := instant.UTC()
	zone := strings.ToLower(strings.TrimSpace(scope.Zone))
	region := strings.ToLower(strings.TrimSpace(scope.Region))

	decision := domain.AdmissionDecision{
		Allowed:     true,
		Mode:        domain.AdmissionModeAlwaysOpen,
		EvaluatedAt: now,
		Zone:        zone,
		Region:      region,
	}

	views := matchingCalendars(calendars, windows, zone, region)
	if len(views) == 0 {
		return decision
	}

	calendarIDs := make([]string, 0, len(views))
	versions := make(map[string]uint64, len(views))
	for _, view := range views {
		calendarIDs = append(calendarIDs, view.calendar.ID)
		versions[view.calendar.ID] = view.calendar.Version
	}
	sort.Strings(calendarIDs)
	decision.CalendarIDs = calendarIDs
	decision.CalendarVersions = versions

	type hit struct {
		entry    domain.MaintenanceWindow
		calendar domain.Calendar
	}
	open := make([]hit, 0)
	considered := make(map[string]struct{})
	// Suspensions are evaluated first: any covering suspension vetoes,
	// regardless of overlapping open windows.
	for _, view := range views {
		for _, entry := range view.entries {
			intervals, err := EntryIntervals(entry, view.timezone, now)
			if err != nil {
				// A stored calendar always has a validated IANA timezone,
				// but stay deterministic if tzdata rejects it: the entry
				// simply contributes no interval.
				continue
			}
			covering := false
			for _, interval := range intervals {
				if interval.covers(now) {
					covering = true
					break
				}
			}
			if !covering {
				continue
			}
			considered[entry.ID] = struct{}{}
			if entry.Kind == domain.EntrySuspension {
				decision.Allowed = false
				decision.Mode = domain.AdmissionModeScheduled
				decision.Reason = domain.AdmissionSuspended
				decision.SuspensionWindowID = entry.ID
				decision.SuspensionCalendarID = view.calendar.ID
				decision.ConsideredWindowIDs = sortedKeys(considered)
				return decision
			}
			open = append(open, hit{entry: entry, calendar: view.calendar})
		}
	}

	if len(open) == 0 {
		decision.Allowed = false
		decision.Mode = domain.AdmissionModeScheduled
		decision.Reason = domain.AdmissionOutsideWindow
		decision.ConsideredWindowIDs = sortedKeys(considered)
		return decision
	}

	sort.SliceStable(open, func(i, j int) bool {
		return lessWindow(open[i].entry, open[j].entry)
	})
	winner := open[0].entry
	winnerCalendar := open[0].calendar
	decision.Allowed = true
	decision.Mode = domain.AdmissionModeScheduled
	decision.MatchedWindowID = winner.ID
	decision.MatchedCalendarID = winnerCalendar.ID
	decision.ConsideredWindowIDs = sortedKeys(considered)
	return decision
}

// lessWindow defines the deterministic overlap ordering.
func lessWindow(left, right domain.MaintenanceWindow) bool {
	if left.Priority != right.Priority {
		return left.Priority < right.Priority
	}
	// Specific (one-time) beats recurring (weekly).
	if left.Schedule != right.Schedule {
		return left.Schedule == domain.ScheduleOneTime
	}
	if durationRank(left) != durationRank(right) {
		return durationRank(left) > durationRank(right)
	}
	return left.ID < right.ID
}

// durationRank orders longer windows first. One-time windows use their exact
// duration; weekly windows use their nominal duration in seconds.
func durationRank(entry domain.MaintenanceWindow) int64 {
	if entry.OneTime != nil {
		return int64(entry.OneTime.EndAt.Sub(entry.OneTime.StartAt).Seconds())
	}
	if entry.Weekly != nil {
		return int64(entry.Weekly.DurationMinutes) * 60
	}
	return 0
}

// matchingCalendars selects calendars whose scope contains the action and
// groups their live entries. A calendar with no scope entries is global.
func matchingCalendars(calendars []domain.Calendar, windows []domain.MaintenanceWindow, zone, region string) []calendarView {
	entriesByCalendar := make(map[string][]domain.MaintenanceWindow)
	for _, entry := range windows {
		if entry.ArchivedAt != nil {
			continue
		}
		entriesByCalendar[entry.CalendarID] = append(entriesByCalendar[entry.CalendarID], entry)
	}
	views := make([]calendarView, 0)
	for _, calendar := range calendars {
		if calendar.ArchivedAt != nil {
			continue
		}
		if !scopeContains(calendar.ScopeZones, zone) {
			continue
		}
		if !scopeContains(calendar.ScopeRegions, region) {
			continue
		}
		views = append(views, calendarView{
			calendar: calendar,
			timezone: calendar.Timezone,
			entries:  entriesByCalendar[calendar.ID],
		})
	}
	sort.Slice(views, func(i, j int) bool {
		return views[i].calendar.ID < views[j].calendar.ID
	})
	return views
}

// scopeContains treats an empty scope as a wildcard. For plan activation
// Region is empty; region-specific calendars therefore never match the
// activation action, while they still match node allocation in that region.
func scopeContains(scope []string, value string) bool {
	if len(scope) == 0 {
		return true
	}
	if value == "" {
		return false
	}
	for _, item := range scope {
		if item == value {
			return true
		}
	}
	return false
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

// FromState evaluates admission directly against a state snapshot, which is
// how activation and allocation share the same interpretation while holding
// the store write lock.
func FromState(state *domain.State, scope Scope, instant time.Time) domain.AdmissionDecision {
	calendars := make([]domain.Calendar, 0, len(state.Calendars))
	for _, calendar := range state.Calendars {
		calendars = append(calendars, calendar)
	}
	windows := make([]domain.MaintenanceWindow, 0, len(state.Windows))
	for _, entry := range state.Windows {
		windows = append(windows, entry)
	}
	return Evaluate(calendars, windows, scope, instant)
}

// DeniedError turns a denied decision into the stable domain error surfaced
// by activation and allocation.
func DeniedError(decision domain.AdmissionDecision) *domain.DomainError {
	message := "rollout is outside the configured maintenance window"
	details := map[string]any{
		"zone":              decision.Zone,
		"evaluated_at":      decision.EvaluatedAt,
		"calendar_ids":      decision.CalendarIDs,
		"matched_window_id": decision.MatchedWindowID,
	}
	if decision.Region != "" {
		details["region"] = decision.Region
	}
	if decision.Reason == domain.AdmissionSuspended {
		message = "rollout is blocked by a maintenance suspension"
		details["suspension_window_id"] = decision.SuspensionWindowID
		details["suspension_calendar_id"] = decision.SuspensionCalendarID
	}
	return domain.NewError(domain.CodeOutsideWindow, message, details)
}
