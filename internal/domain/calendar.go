package domain

import "time"

// CalendarEntryKind distinguishes rollout windows from suspensions.
type CalendarEntryKind string

const (
	EntryWindow     CalendarEntryKind = "window"
	EntrySuspension CalendarEntryKind = "suspension"
)

// WindowSchedule selects how a calendar entry is interpreted.
type WindowSchedule string

const (
	ScheduleOneTime WindowSchedule = "one_time"
	ScheduleWeekly  WindowSchedule = "weekly"
)

// DSTGapPolicy defines what happens to a weekly anchor that lands on a
// non-existent local wall clock (a spring-forward gap).
type DSTGapPolicy string

const (
	// GapShift moves the anchor forward across the gap.
	GapShift DSTGapPolicy = "shift"
	// GapReject skips the occurrence for that day.
	GapReject DSTGapPolicy = "reject"
)

// Admission reason codes are stable for API consumers.
const (
	AdmissionModeScheduled  = "scheduled"
	AdmissionModeAlwaysOpen = "always_open"
	AdmissionOutsideWindow  = "outside_window"
	AdmissionSuspended      = "suspended"
)

// Calendar scopes maintenance time to a timezone and a set of zones and
// regions. Empty scope slices mean "all". Calendars are never hard deleted:
// archiving keeps history reconstructable and removes them from admission.
type Calendar struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Timezone     string     `json:"timezone"`
	ScopeZones   []string   `json:"scope_zones"`
	ScopeRegions []string   `json:"scope_regions"`
	Version      uint64     `json:"version"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	ArchivedAt   *time.Time `json:"archived_at,omitempty"`
}

// OneTimeRange is an absolute half-open interval in UTC.
type OneTimeRange struct {
	StartAt time.Time `json:"start_at"`
	EndAt   time.Time `json:"end_at"`
}

// WeeklySchedule is a recurring anchor expressed in the owning calendar's
// local wall clock. Weekday uses time.Weekday numbering (0=Sunday).
// StartMinute is minutes since local midnight; the occurrence ends exactly
// DurationMinutes later as an instant, so DST shifts never change semantics.
type WeeklySchedule struct {
	Weekday         time.Weekday `json:"weekday"`
	StartMinute     int          `json:"start_minute"`
	DurationMinutes int          `json:"duration_minutes"`
	GapPolicy       DSTGapPolicy `json:"gap_policy"`
}

// MaintenanceWindow is an immutable calendar entry (window or suspension).
// Changing it means archiving the old entry and registering a new one, so
// admission snapshots never change meaning underneath an in-flight rollout.
type MaintenanceWindow struct {
	ID         string            `json:"id"`
	CalendarID string            `json:"calendar_id"`
	Name       string            `json:"name"`
	Kind       CalendarEntryKind `json:"kind"`
	Schedule   WindowSchedule    `json:"schedule"`
	Priority   int               `json:"priority"`
	OneTime    *OneTimeRange     `json:"one_time,omitempty"`
	Weekly     *WeeklySchedule   `json:"weekly,omitempty"`
	Version    uint64            `json:"version"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	ArchivedAt *time.Time        `json:"archived_at,omitempty"`
}

// AdmissionDecision is the frozen interpretation of the calendar at one
// admission point (plan activation or lease allocation). It is stored on the
// resulting plan or lease group and is never recomputed for that approval.
type AdmissionDecision struct {
	Allowed              bool              `json:"allowed"`
	Mode                 string            `json:"mode"`
	Reason               string            `json:"reason,omitempty"`
	EvaluatedAt          time.Time         `json:"evaluated_at"`
	Zone                 string            `json:"zone"`
	Region               string            `json:"region,omitempty"`
	MatchedWindowID      string            `json:"matched_window_id,omitempty"`
	MatchedCalendarID    string            `json:"matched_calendar_id,omitempty"`
	SuspensionWindowID   string            `json:"suspension_window_id,omitempty"`
	SuspensionCalendarID string            `json:"suspension_calendar_id,omitempty"`
	CalendarIDs          []string          `json:"calendar_ids,omitempty"`
	CalendarVersions     map[string]uint64 `json:"calendar_versions,omitempty"`
	ConsideredWindowIDs  []string          `json:"considered_window_ids,omitempty"`
}

// CalendarDetail bundles a calendar with its live (non-archived) entries.
type CalendarDetail struct {
	Calendar
	Windows []MaintenanceWindow `json:"windows"`
}

type CalendarPage struct {
	Items []Calendar `json:"items"`
	Meta  PageMeta   `json:"meta"`
}

type CalendarInput struct {
	Name         string   `json:"name"`
	Timezone     string   `json:"timezone"`
	ScopeZones   []string `json:"scope_zones"`
	ScopeRegions []string `json:"scope_regions"`
}

type OneTimeInput struct {
	StartAt time.Time `json:"start_at"`
	EndAt   time.Time `json:"end_at"`
}

type WeeklyInput struct {
	Weekday         int    `json:"weekday"`
	Start           string `json:"start"`
	DurationMinutes int    `json:"duration_minutes"`
	GapPolicy       string `json:"gap_policy"`
}

type WindowInput struct {
	Name     string        `json:"name"`
	Kind     string        `json:"kind"`
	Schedule string        `json:"schedule"`
	Priority int           `json:"priority"`
	OneTime  *OneTimeInput `json:"one_time,omitempty"`
	Weekly   *WeeklyInput  `json:"weekly,omitempty"`
}
