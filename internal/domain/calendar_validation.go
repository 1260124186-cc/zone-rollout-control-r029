package domain

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxCalendarNameRunes = 120
	MaxWindowNameRunes   = 120
	MaxCalendarScopes    = 100
	MaxPriority          = 1000
	MinOneTimeSeconds    = 60
	MaxOneTimeSeconds    = 7 * 24 * 3600
	MinWeeklyMinutes     = 15
	MaxWeeklyMinutes     = 24 * 60
)

func NormalizeCalendarInput(input CalendarInput) CalendarInput {
	input.Name = strings.TrimSpace(input.Name)
	input.Timezone = strings.TrimSpace(input.Timezone)
	input.ScopeZones = normalizeScopeList(input.ScopeZones)
	input.ScopeRegions = normalizeScopeList(input.ScopeRegions)
	return input
}

func ValidateCalendarInput(input CalendarInput) error {
	input = NormalizeCalendarInput(input)
	details := make(map[string]any)
	if count := utf8.RuneCountInString(input.Name); count < 2 || count > MaxCalendarNameRunes {
		details["name"] = "must contain between 2 and 120 characters"
	}
	if _, err := time.LoadLocation(input.Timezone); err != nil {
		details["timezone"] = "must be a valid IANA timezone"
	}
	if len(input.ScopeZones) > MaxCalendarScopes {
		details["scope_zones"] = "may contain at most 100 items"
	}
	if len(input.ScopeRegions) > MaxCalendarScopes {
		details["scope_regions"] = "may contain at most 100 items"
	}
	for index, zone := range input.ScopeZones {
		if err := validateCanonicalName(zone); err != nil {
			details["scope_zones["+strconv.Itoa(index)+"]"] = err.Error()
		}
	}
	for index, region := range input.ScopeRegions {
		if count := utf8.RuneCountInString(region); count < 2 || count > MaxRegionRunes {
			details["scope_regions["+strconv.Itoa(index)+"]"] = "must contain between 2 and 48 characters"
		}
	}
	if len(details) > 0 {
		return Validation("calendar input is invalid", details)
	}
	return nil
}

func NormalizeWindowInput(input WindowInput) WindowInput {
	input.Name = strings.TrimSpace(input.Name)
	input.Kind = strings.TrimSpace(strings.ToLower(input.Kind))
	input.Schedule = strings.TrimSpace(strings.ToLower(input.Schedule))
	if input.Weekly != nil {
		input.Weekly.Start = strings.TrimSpace(input.Weekly.Start)
		input.Weekly.GapPolicy = strings.TrimSpace(strings.ToLower(input.Weekly.GapPolicy))
	}
	return input
}

func ValidateWindowInput(input WindowInput) error {
	input = NormalizeWindowInput(input)
	details := make(map[string]any)
	if count := utf8.RuneCountInString(input.Name); count < 2 || count > MaxWindowNameRunes {
		details["name"] = "must contain between 2 and 120 characters"
	}
	switch CalendarEntryKind(input.Kind) {
	case EntryWindow, EntrySuspension:
	default:
		details["kind"] = "must be window or suspension"
	}
	switch WindowSchedule(input.Schedule) {
	case ScheduleOneTime, ScheduleWeekly:
	default:
		details["schedule"] = "must be one_time or weekly"
	}
	if input.Priority < 0 || input.Priority > MaxPriority {
		details["priority"] = "must be between 0 and 1000"
	}
	switch WindowSchedule(input.Schedule) {
	case ScheduleOneTime:
		validateOneTime(input, details)
	case ScheduleWeekly:
		validateWeekly(input, details)
	}
	if len(details) > 0 {
		return Validation("maintenance window input is invalid", details)
	}
	return nil
}

func validateOneTime(input WindowInput, details map[string]any) {
	if input.OneTime == nil {
		details["one_time"] = "is required for one_time schedule"
		return
	}
	start := input.OneTime.StartAt
	end := input.OneTime.EndAt
	if start.IsZero() || end.IsZero() {
		details["one_time"] = "start_at and end_at are required"
		return
	}
	duration := end.Sub(start)
	if duration <= 0 {
		details["one_time.end_at"] = "must be after start_at"
		return
	}
	if duration.Seconds() < MinOneTimeSeconds || duration.Seconds() > MaxOneTimeSeconds {
		details["one_time.end_at"] = "duration must be between 1 minute and 7 days"
	}
}

func validateWeekly(input WindowInput, details map[string]any) {
	if input.Weekly == nil {
		details["weekly"] = "is required for weekly schedule"
		return
	}
	weekly := input.Weekly
	if weekly.Weekday < 0 || weekly.Weekday > 6 {
		details["weekly.weekday"] = "must be between 0 (Sunday) and 6 (Saturday)"
	}
	if _, ok := parseHHMM(weekly.Start); !ok {
		details["weekly.start"] = "must be HH:MM in local calendar time"
	}
	if weekly.DurationMinutes < MinWeeklyMinutes || weekly.DurationMinutes > MaxWeeklyMinutes {
		details["weekly.duration_minutes"] = "must be between 15 and 1440 minutes"
	}
	switch DSTGapPolicy(weekly.GapPolicy) {
	case GapShift, GapReject:
	default:
		details["weekly.gap_policy"] = "must be shift or reject"
	}
}

// ParseWeeklyStart converts a validated HH:MM anchor to minutes since midnight.
func ParseWeeklyStart(value string) (int, bool) {
	return parseHHMM(strings.TrimSpace(value))
}

func parseHHMM(value string) (int, bool) {
	if len(value) != 5 || value[2] != ':' {
		return 0, false
	}
	hour, errHour := strconv.Atoi(value[0:2])
	minute, errMinute := strconv.Atoi(value[3:5])
	if errHour != nil || errMinute != nil {
		return 0, false
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

func normalizeScopeList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(strings.ToLower(value))
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	sortStrings(result)
	return result
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for current := index; current > 0 && values[current-1] > values[current]; current-- {
			values[current-1], values[current] = values[current], values[current-1]
		}
	}
}
