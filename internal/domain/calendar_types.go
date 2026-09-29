package domain

import "time"

// WindowMode determines whether a calendar entry opens rollout time or blocks it.
type WindowMode string

const (
	// WindowPermit marks a maintenance/rollout window during which activation
	// and lease claiming are allowed.
	WindowPermit WindowMode = "permit"
	// WindowPause marks a suspension period that overrides every overlapping
	// permit window.
	WindowPause WindowMode = "pause"
)

// WindowSchedule selects whether a window fires once or repeats weekly.
type WindowSchedule string

const (
	WindowOnce   WindowSchedule = "once"
	WindowWeekly WindowSchedule = "weekly"
)

// WindowEntry is an immutable-once-created, versioned calendar entry bound to a
// plan. Pause windows use the same structure with mode = "pause".
type WindowEntry struct {
	ID        string         `json:"id"`
	PlanID    string         `json:"plan_id"`
	Mode      WindowMode     `json:"mode"`
	Schedule  WindowSchedule `json:"schedule"`
	Timezone  string         `json:"timezone"`
	StartsAt  time.Time      `json:"starts_at,omitempty"`
	EndsAt    time.Time      `json:"ends_at,omitempty"`
	Weekdays  []time.Weekday `json:"weekdays,omitempty"`
	StartTime string         `json:"start_time,omitempty"`
	EndTime   string         `json:"end_time,omitempty"`
	Regions   []string       `json:"regions,omitempty"`
	Note      string         `json:"note,omitempty"`
	Version   uint64         `json:"version"`
	Deleted   bool           `json:"deleted,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// WindowBasis identifies the single permit entry an approval was based on.
type WindowBasis struct {
	WindowID  string    `json:"window_id"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	Schedule  string    `json:"schedule"`
	Timezone  string    `json:"timezone"`
	Region    string    `json:"region,omitempty"`
	LocalTime string    `json:"local_time"`
}

// WindowDecision is the immutable interpretation captured at the moment a plan
// is activated or a lease group is allocated. Replaying the same request or
// changing the calendar afterwards never rewrites it.
type WindowDecision struct {
	Allowed         bool          `json:"allowed"`
	Reason          string        `json:"reason,omitempty"`
	Scope           string        `json:"scope"`
	Mode            string        `json:"mode"`
	EvaluatedAt     time.Time     `json:"evaluated_at"`
	PlanID          string        `json:"plan_id,omitempty"`
	Zone            string        `json:"zone"`
	Region          string        `json:"region,omitempty"`
	Basis           *WindowBasis  `json:"basis,omitempty"`
	BlockingPauseID string        `json:"blocking_pause_id,omitempty"`
	NextOpeningAt   *time.Time    `json:"next_opening_at,omitempty"`
	Windows         []WindowEntry `json:"windows"`
}
