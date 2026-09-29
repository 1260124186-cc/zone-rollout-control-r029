package domain

import (
	"sort"
	"time"
)

// DecisionScope labels which scope a calendar evaluation used. Plan activation
// always uses the plan-wide scope; lease allocation uses the requested region.
const (
	DecisionScopePlan   = "plan"
	DecisionScopeRegion = "region"
)

// nextOpeningHorizon bounds the search for the next opening. Every weekly
// occurrence repeats at least once per week, so anything open eventually
// appears well within this window.
const nextOpeningHorizon = 400 * 24 * time.Hour

// EvaluateCalendar is the single source of time interpretation for scheduled
// activation and node allocation. It is a pure function: the same state, the
// same instant and the same scope always produce the same decision.
//
// Determinism rules:
//   - A plan without any calendar entry is open at all hours.
//   - Applicability is opt-in per scope: an empty regions list applies
//     plan-wide and to every region; a populated list applies only to those
//     regions. Plan-wide activation ignores region-scoped entries.
//   - Default deny once a calendar exists: the instant must be covered by at
//     least one non-deleted permit window.
//   - Pauses always win. A pause covering the instant denies even when a
//     permit covers it.
//   - Weekly occurrences are wall-clock intervals replayed in the entry
//     timezone. Go's time.Date resolves DST gaps and repeats deterministically
//     per the system tzdata, and a duration-based end naturally follows the
//     offset change across a transition.
//   - Interval endpoints are closed-open [start, end), so adjacent windows
//     neither overlap nor leave a gap.
//   - When several permits cover the instant, the earliest-occurring one is
//     the recorded basis, ties broken by window ID.
func EvaluateCalendar(
	state *State,
	planID string,
	zone string,
	scope string,
	region string,
	at time.Time,
) WindowDecision {
	now := at.UTC()
	decision := WindowDecision{
		Scope:       scope,
		Mode:        "scheduled",
		EvaluatedAt: now,
		PlanID:      planID,
		Zone:        zone,
		Region:      region,
		Windows:     []WindowEntry{},
	}

	entries := planWindows(state, planID)
	if len(entries) == 0 {
		decision.Allowed = true
		decision.Reason = WindowReasonUnconfigured
		decision.Mode = "all_hours"
		return decision
	}

	applicable := make([]WindowEntry, 0, len(entries))
	for _, entry := range entries {
		if windowApplies(entry, scope, region) {
			applicable = append(applicable, entry)
		}
	}
	sort.Slice(applicable, func(i, j int) bool { return applicable[i].ID < applicable[j].ID })
	decision.Windows = append(decision.Windows, applicable...)

	pauses := make([]WindowEntry, 0)
	permits := make([]WindowEntry, 0)
	for _, entry := range applicable {
		switch entry.Mode {
		case WindowPause:
			pauses = append(pauses, entry)
		case WindowPermit:
			permits = append(permits, entry)
		}
	}

	type match struct {
		entry WindowEntry
		start time.Time
		end   time.Time
	}
	covering := make([]match, 0, len(permits))
	for _, permit := range permits {
		if start, end, ok := occurrenceCovering(permit, now); ok {
			covering = append(covering, match{entry: permit, start: start, end: end})
		}
	}
	if len(covering) == 0 {
		decision.Allowed = false
		decision.Reason = WindowReasonNoPermit
		decision.NextOpeningAt = nextOpening(permits, pauses, now)
		return decision
	}
	sort.Slice(covering, func(i, j int) bool {
		if !covering[i].start.Equal(covering[j].start) {
			return covering[i].start.Before(covering[j].start)
		}
		if !covering[i].end.Equal(covering[j].end) {
			return covering[i].end.Before(covering[j].end)
		}
		return covering[i].entry.ID < covering[j].entry.ID
	})
	chosen := covering[0]

	if pause, ok := pauseCovering(pauses, now); ok {
		decision.Allowed = false
		decision.Reason = WindowReasonPaused
		decision.BlockingPauseID = pause.ID
		decision.NextOpeningAt = nextOpening(permits, pauses, now)
		return decision
	}

	decision.Allowed = true
	location, _ := time.LoadLocation(chosen.entry.Timezone)
	decision.Basis = &WindowBasis{
		WindowID:  chosen.entry.ID,
		StartsAt:  chosen.start,
		EndsAt:    chosen.end,
		Schedule:  string(chosen.entry.Schedule),
		Timezone:  chosen.entry.Timezone,
		Region:    matchingRegion(chosen.entry, scope, region),
		LocalTime: now.In(location).Format("2006-01-02T15:04:05Z07:00"),
	}
	return decision
}

// ClosedError converts a deny decision into the stable domain error surfaced by
// activation and lease allocation.
func ClosedError(decision WindowDecision) *DomainError {
	details := map[string]any{
		"plan_id":   decision.PlanID,
		"scope":     decision.Scope,
		"reason":    decision.Reason,
		"evaluated": decision.EvaluatedAt,
	}
	if decision.Region != "" {
		details["region"] = decision.Region
	}
	if decision.BlockingPauseID != "" {
		details["pause_id"] = decision.BlockingPauseID
	}
	if decision.NextOpeningAt != nil {
		details["next_opening_at"] = *decision.NextOpeningAt
	}
	message := "rollout window is closed"
	switch decision.Reason {
	case WindowReasonPaused:
		message = "rollout is paused by a suspension window"
	case WindowReasonNoPermit:
		message = "no active rollout window covers this time"
	}
	return NewError(CodeWindowClosed, message, details)
}

func planWindows(state *State, planID string) []WindowEntry {
	entries := make([]WindowEntry, 0)
	for _, entry := range state.Windows {
		if entry.PlanID == planID && !entry.Deleted {
			entries = append(entries, entry)
		}
	}
	return entries
}

func windowApplies(entry WindowEntry, scope string, region string) bool {
	if len(entry.Regions) == 0 {
		return true
	}
	if scope != DecisionScopeRegion {
		return false
	}
	for _, candidate := range entry.Regions {
		if candidate == region {
			return true
		}
	}
	return false
}

func matchingRegion(entry WindowEntry, scope string, region string) string {
	if len(entry.Regions) == 0 {
		return ""
	}
	if scope == DecisionScopeRegion {
		return region
	}
	return entry.Regions[0]
}

// occurrenceCovering returns the concrete [start, end) UTC interval of the
// window occurrence that contains at, when one exists.
func occurrenceCovering(entry WindowEntry, at time.Time) (time.Time, time.Time, bool) {
	location, err := time.LoadLocation(entry.Timezone)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	if entry.Schedule == WindowOnce {
		if !at.Before(entry.StartsAt) && at.Before(entry.EndsAt) {
			return entry.StartsAt.UTC(), entry.EndsAt.UTC(), true
		}
		return time.Time{}, time.Time{}, false
	}
	startHour, startMinute, _ := parseWallClock(entry.StartTime)
	endHour, endMinute, _ := parseWallClock(entry.EndTime)
	duration := clockDuration(startHour, startMinute, endHour, endMinute)
	if duration <= 0 {
		// Cross-midnight occurrence: the interval spills into the next day.
		duration += 24 * time.Hour
	}
	local := at.In(location)
	// An occurrence can only start on its own local date or the immediately
	// preceding local date (when it crosses midnight). Anchor both at local
	// midnight so DST shifts never move an anchor onto a different weekday, and
	// dedupe identical UTC starts: a spring-forward gap can normalize two
	// local anchors to the same instant.
	anchors := []time.Time{
		time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location),
		time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location).AddDate(0, 0, -1),
	}
	seenStart := make(map[int64]struct{}, len(anchors))
	for _, midnight := range anchors {
		if !weekdayListed(entry.Weekdays, midnight.Weekday()) {
			continue
		}
		start := time.Date(midnight.Year(), midnight.Month(), midnight.Day(), startHour, startMinute, 0, 0, location)
		key := start.UTC().UnixNano()
		if _, seen := seenStart[key]; seen {
			continue
		}
		seenStart[key] = struct{}{}
		end := start.Add(duration)
		if !at.Before(start.UTC()) && at.Before(end.UTC()) {
			return start.UTC(), end.UTC(), true
		}
	}
	return time.Time{}, time.Time{}, false
}

func weekdayListed(weekdays []time.Weekday, day time.Weekday) bool {
	for _, candidate := range weekdays {
		if candidate == day {
			return true
		}
	}
	return false
}

func clockDuration(startHour, startMinute, endHour, endMinute int) time.Duration {
	return time.Duration(endHour*60+endMinute-startHour*60-startMinute) * time.Minute
}

func pauseCovering(pauses []WindowEntry, at time.Time) (WindowEntry, bool) {
	sort.Slice(pauses, func(i, j int) bool { return pauses[i].ID < pauses[j].ID })
	for _, pause := range pauses {
		if _, _, ok := occurrenceCovering(pause, at); ok {
			return pause, true
		}
	}
	return WindowEntry{}, false
}

// nextOpening finds the earliest instant strictly after now at which some
// permit occurrence starts and is not itself covered by a pause. If a pause
// only trims the beginning of a permit, the first uncovered instant inside the
// same occurrence is returned. It returns nil when nothing opens within the
// bounded horizon.
func nextOpening(permits []WindowEntry, pauses []WindowEntry, now time.Time) *time.Time {
	horizon := now.Add(nextOpeningHorizon)
	var best time.Time
	found := false
	consider := func(candidate time.Time) {
		if candidate.Before(horizon) && (!found || candidate.Before(best)) {
			best = candidate
			found = true
		}
	}
	for _, permit := range permits {
		for _, occurrence := range upcomingOccurrences(permit, now, horizon) {
			candidate := firstUncoveredInstant(occurrence.start, occurrence.end, pauses, now)
			if candidate.IsZero() {
				continue
			}
			consider(candidate)
		}
	}
	if !found {
		return nil
	}
	pointer := best.UTC()
	return &pointer
}

type interval struct {
	start time.Time
	end   time.Time
}

func upcomingOccurrences(entry WindowEntry, now time.Time, horizon time.Time) []interval {
	if entry.Schedule == WindowOnce {
		if entry.EndsAt.After(now) {
			return []interval{{start: entry.StartsAt.UTC(), end: entry.EndsAt.UTC()}}
		}
		return nil
	}
	location, err := time.LoadLocation(entry.Timezone)
	if err != nil {
		return nil
	}
	startHour, startMinute, _ := parseWallClock(entry.StartTime)
	endHour, endMinute, _ := parseWallClock(entry.EndTime)
	duration := clockDuration(startHour, startMinute, endHour, endMinute)
	if duration <= 0 {
		duration += 24 * time.Hour
	}

	// Scan forward by local dates anchored at local midnight so DST shifts
	// cannot drop or duplicate a day. Only the first occurrence may start at
	// or before now.
	local := now.In(location)
	firstMidnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	results := make([]interval, 0)
	for dayOffset := 0; dayOffset <= int(nextOpeningHorizon/(24*time.Hour))+1; dayOffset++ {
		day := firstMidnight.AddDate(0, 0, dayOffset)
		if !weekdayListed(entry.Weekdays, day.Weekday()) {
			continue
		}
		start := time.Date(day.Year(), day.Month(), day.Day(), startHour, startMinute, 0, 0, location)
		end := start.Add(duration)
		if !end.UTC().After(now) {
			continue
		}
		if !start.UTC().Before(horizon) {
			break
		}
		results = append(results, interval{start: start.UTC(), end: end.UTC()})
	}
	return results
}

// firstUncoveredInstant walks a permit occurrence past any intersecting pause
// occurrences and returns the earliest instant at or after now that is not
// blocked. Pauses are merged in start order so chained overlaps resolve
// deterministically. It returns zero when the whole occurrence is blocked.
func firstUncoveredInstant(start time.Time, end time.Time, pauses []WindowEntry, now time.Time) time.Time {
	cursor := start
	if cursor.Before(now) {
		cursor = now
	}
	type span struct {
		start time.Time
		end   time.Time
	}
	spans := make([]span, 0, len(pauses))
	for _, pause := range pauses {
		for _, occurrence := range upcomingOccurrences(pause, cursor, end) {
			if occurrence.start.Before(end) && occurrence.end.After(cursor) {
				spans = append(spans, span{start: occurrence.start, end: occurrence.end})
			}
		}
	}
	sort.Slice(spans, func(i, j int) bool {
		if !spans[i].start.Equal(spans[j].start) {
			return spans[i].start.Before(spans[j].start)
		}
		return spans[i].end.Before(spans[j].end)
	})
	for _, block := range spans {
		if block.end.Before(cursor) || block.end.Equal(cursor) {
			continue
		}
		if block.start.After(cursor) {
			break
		}
		cursor = block.end
	}
	if !cursor.Before(end) {
		return time.Time{}
	}
	return cursor
}
