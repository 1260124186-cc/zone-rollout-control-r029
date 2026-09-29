package domain

import (
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxWindowNoteRunes = 200
	MaxWindowRegions   = 32
)

// WindowInput creates or fully replaces a calendar entry. Timezone is always an
// IANA location (for example "Asia/Shanghai" or "America/New_York"). One-off
// windows carry starts_at/ends_at RFC3339 instants; weekly windows carry
// weekdays plus start_time/end_time wall-clock values interpreted in timezone.
type WindowInput struct {
	Mode      *string    `json:"mode"`
	Schedule  *string    `json:"schedule"`
	Timezone  string     `json:"timezone"`
	StartsAt  *time.Time `json:"starts_at,omitempty"`
	EndsAt    *time.Time `json:"ends_at,omitempty"`
	Weekdays  []int      `json:"weekdays,omitempty"`
	StartTime string     `json:"start_time,omitempty"`
	EndTime   string     `json:"end_time,omitempty"`
	Regions   []string   `json:"regions,omitempty"`
	Note      string     `json:"note,omitempty"`
}

// NormalizedWindowInput holds the validated, canonical form of WindowInput.
type NormalizedWindowInput struct {
	Mode      WindowMode
	Schedule  WindowSchedule
	Timezone  string
	StartsAt  time.Time
	EndsAt    time.Time
	Weekdays  []time.Weekday
	StartTime string
	EndTime   string
	Regions   []string
	Note      string
}

// Window reasons are stable strings shared by decisions and error details.
const (
	WindowReasonUnconfigured = "unconfigured"
	WindowReasonNoPermit     = "no_matching_permit_window"
	WindowReasonPaused       = "paused"
)

// NormalizeWindowInput trims and canonicalizes raw input without loading any
// location. Location loading and semantic checks happen in ValidateWindowInput.
func NormalizeWindowInput(input WindowInput) NormalizedWindowInput {
	normalized := NormalizedWindowInput{
		Timezone:  strings.TrimSpace(input.Timezone),
		StartTime: strings.TrimSpace(input.StartTime),
		EndTime:   strings.TrimSpace(input.EndTime),
		Note:      strings.TrimSpace(input.Note),
	}
	if input.Mode != nil {
		normalized.Mode = WindowMode(strings.ToLower(strings.TrimSpace(*input.Mode)))
	}
	if input.Schedule != nil {
		normalized.Schedule = WindowSchedule(strings.ToLower(strings.TrimSpace(*input.Schedule)))
	}
	if input.StartsAt != nil {
		normalized.StartsAt = input.StartsAt.UTC()
	}
	if input.EndsAt != nil {
		normalized.EndsAt = input.EndsAt.UTC()
	}
	for _, day := range input.Weekdays {
		normalized.Weekdays = append(normalized.Weekdays, time.Weekday(day))
	}
	for _, region := range input.Regions {
		region = strings.TrimSpace(strings.ToLower(region))
		if region != "" {
			normalized.Regions = append(normalized.Regions, region)
		}
	}
	sort.Strings(normalized.Regions)
	normalized.Regions = dedupeStrings(normalized.Regions)
	return normalized
}

func dedupeStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

// ValidateWindowInput performs every check that does not depend on stored
// state, including loading the IANA timezone and resolving wall-clock strings.
// Cross-midnight weekly windows are allowed: an end earlier than the start
// means the occurrence spills into the following day.
func ValidateWindowInput(input NormalizedWindowInput) error {
	details := make(map[string]any)
	if input.Mode != WindowPermit && input.Mode != WindowPause {
		details["mode"] = "must be permit or pause"
	}
	if input.Schedule != WindowOnce && input.Schedule != WindowWeekly {
		details["schedule"] = "must be once or weekly"
	}
	location, err := time.LoadLocation(input.Timezone)
	if err != nil || input.Timezone == "" {
		details["timezone"] = "must be a valid IANA timezone name"
	}
	if count := utf8.RuneCountInString(input.Note); count > MaxWindowNoteRunes {
		details["note"] = "must be at most 200 characters"
	}
	if len(input.Regions) > MaxWindowRegions {
		details["regions"] = "must contain at most 32 items"
	}
	for _, region := range input.Regions {
		if count := utf8.RuneCountInString(region); count < 2 || count > MaxRegionRunes {
			details["regions"] = "each region must contain between 2 and 48 characters"
			break
		}
	}

	if input.Schedule == WindowOnce {
		if input.StartsAt.IsZero() {
			details["starts_at"] = "is required for one-time windows"
		}
		if input.EndsAt.IsZero() {
			details["ends_at"] = "is required for one-time windows"
		}
		if !input.StartsAt.IsZero() && !input.EndsAt.IsZero() && !input.EndsAt.After(input.StartsAt) {
			details["ends_at"] = "must be after starts_at"
		}
		if len(input.Weekdays) != 0 {
			details["weekdays"] = "must be omitted for one-time windows"
		}
		if input.StartTime != "" || input.EndTime != "" {
			details["start_time"] = "must be omitted for one-time windows"
		}
	}
	if input.Schedule == WindowWeekly {
		if !input.StartsAt.IsZero() || !input.EndsAt.IsZero() {
			details["starts_at"] = "must be omitted for weekly windows"
		}
		if len(input.Weekdays) == 0 {
			details["weekdays"] = "must contain at least one weekday"
		}
		seen := make(map[time.Weekday]struct{}, len(input.Weekdays))
		for _, day := range input.Weekdays {
			if day < time.Sunday || day > time.Saturday {
				details["weekdays"] = "each weekday must be between 0 (Sunday) and 6 (Saturday)"
				break
			}
			if _, dup := seen[day]; dup {
				details["weekdays"] = "must not contain duplicated weekdays"
				break
			}
			seen[day] = struct{}{}
		}
		startHour, startMinute, startOK := parseWallClock(input.StartTime)
		endHour, endMinute, endOK := parseWallClock(input.EndTime)
		if !startOK {
			details["start_time"] = "must be HH:MM in 24-hour notation"
		}
		if !endOK {
			details["end_time"] = "must be HH:MM in 24-hour notation"
		}
		if startOK && endOK && startHour == endHour && startMinute == endMinute {
			details["end_time"] = "must differ from start_time; use a one-time window for zero-length pauses"
		}
	}

	if len(details) > 0 {
		return Validation("window input is invalid", details)
	}
	_ = location
	return nil
}

func parseWallClock(value string) (int, int, bool) {
	if len(value) != 5 || value[2] != ':' {
		return 0, 0, false
	}
	hour := int(value[0]-'0')*10 + int(value[1]-'0')
	minute := int(value[3]-'0')*10 + int(value[4]-'0')
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}
