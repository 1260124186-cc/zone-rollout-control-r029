package workflowprobe

import (
	"net/http"
	"time"

	"solo-0014-zone-rollout-control/internal/calendar"
	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/schedule"
	"solo-0014-zone-rollout-control/internal/store"
)

func scheduleScope(zone string) schedule.Scope {
	return schedule.Scope{Zone: zone}
}

func runCalendarWindowAdmission() error {
	h, err := newHarness()
	if err != nil {
		return err
	}
	defer h.close()

	if _, err := enrollNode(h, "resolver-window-01", "east"); err != nil {
		return err
	}

	// 1. No calendar configured: activation is allowed at any time and the
	// frozen decision records always_open.
	openPlan, _, err := createActivePlan(h, "always.example", 3001, 1, 1, 80)
	if err != nil {
		return err
	}
	if openPlan.Admission == nil || openPlan.Admission.Mode != domain.AdmissionModeAlwaysOpen {
		return ensure(false, "plan without calendar must freeze always_open admission")
	}

	// 2. Register a timezone-aware calendar scoped to one zone, with a
	// weekly Friday 10:00-14:00 UTC window.
	var cal domain.Calendar
	if err := h.call(http.MethodPut, "/v1/calendars", domain.CalendarInput{
		Name:       "ops rollout calendar",
		Timezone:   "UTC",
		ScopeZones: []string{"windowed.example"},
	}, &cal); err != nil {
		return err
	}
	var window domain.MaintenanceWindow
	if err := h.call(http.MethodPut, "/v1/calendars/"+cal.ID+"/windows", domain.WindowInput{
		Name:     "friday-morning",
		Kind:     string(domain.EntryWindow),
		Schedule: string(domain.ScheduleWeekly),
		Weekly: &domain.WeeklyInput{
			Weekday:         int(time.Friday),
			Start:           "10:00",
			DurationMinutes: 240,
			GapPolicy:       string(domain.GapShift),
		},
	}, &window); err != nil {
		return err
	}

	// A second zone is untouched by the scoped calendar and stays open.
	var otherPlan domain.RolloutPlan
	if err := h.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone: "unrelated.example", MinimumSuccessRatio: 1, MaximumLatencyMS: 80, ExpectedNodes: 1,
	}, &otherPlan); err != nil {
		return err
	}
	if _, err := addRevision(h, otherPlan.ID, 3002); err != nil {
		return err
	}
	if err := h.call(http.MethodPatch, "/v1/plans/"+otherPlan.ID+"/activate",
		map[string]any{"expected_version": 2}, &otherPlan); err != nil {
		return err
	}

	// 3. Inside the window (harness clock is Friday 12:00 UTC) the scoped
	// plan activates and freezes the scheduled admission.
	plan, _, err := createDraftPlanWithRevision(h, "windowed.example", 3003)
	if err != nil {
		return err
	}
	if err := h.call(http.MethodPatch, "/v1/plans/"+plan.ID+"/activate",
		map[string]any{"expected_version": 2}, &plan); err != nil {
		return err
	}
	if plan.Admission == nil || !plan.Admission.Allowed ||
		plan.Admission.Mode != domain.AdmissionModeScheduled ||
		plan.Admission.MatchedWindowID != window.ID {
		return ensure(false, "activation inside window must freeze matched window")
	}
	frozenMatchedWindow := plan.Admission.MatchedWindowID

	// Allocation uses the same interpretation and freezes its own decision.
	group, err := allocate(h, plan.ID, "cal-lease-0001", "east", 1)
	if err != nil {
		return err
	}
	if group.Admission == nil || !group.Admission.Allowed ||
		group.Admission.MatchedWindowID != window.ID {
		return ensure(false, "allocation must freeze the matched window")
	}

	// Prepare a second draft before moving time; drafts are not gated.
	plan2, revision2, err := createDraftPlanWithRevision(h, "windowed.example", 3004)
	if err != nil {
		return err
	}
	_ = revision2

	// 4. Move outside the window: new activation and new allocation are
	// rejected, but the stored request-key replay still returns the group
	// created inside the window.
	h.clock.Advance(6 * time.Hour)
	err = h.call(http.MethodPatch, "/v1/plans/"+plan2.ID+"/activate",
		map[string]any{"expected_version": 2}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeOutsideWindow)); err != nil {
		return err
	}
	_, err = allocate(h, plan.ID, "cal-lease-0002", "east", 1)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeOutsideWindow)); err != nil {
		return err
	}
	replay, err := allocate(h, plan.ID, "cal-lease-0001", "east", 1)
	if err != nil {
		return err
	}
	if replay.ID != group.ID {
		return ensure(false, "allocation replay after window change must return the frozen group")
	}

	// 5. A suspension vetoes even inside an otherwise open window, checked
	// through the read-only admission endpoint with an explicit instant.
	var suspension domain.MaintenanceWindow
	if err := h.call(http.MethodPut, "/v1/calendars/"+cal.ID+"/windows", domain.WindowInput{
		Name:     "incident-hold",
		Kind:     string(domain.EntrySuspension),
		Schedule: string(domain.ScheduleOneTime),
		OneTime: &domain.OneTimeInput{
			StartAt: parseRFC3339("2026-09-25T11:00:00Z"),
			EndAt:   parseRFC3339("2026-09-25T13:00:00Z"),
		},
	}, &suspension); err != nil {
		return err
	}
	var held domain.AdmissionDecision
	if err := h.call(http.MethodPatch, "/v1/calendar-admission", map[string]string{
		"zone": "windowed.example",
		"at":   "2026-09-25T12:00:00Z",
	}, &held); err != nil {
		return err
	}
	if held.Allowed || held.Reason != domain.AdmissionSuspended ||
		held.SuspensionWindowID != suspension.ID {
		return ensure(false, "suspension must veto the open window deterministically")
	}
	if err := h.call(http.MethodDelete,
		"/v1/calendars/"+cal.ID+"/windows/"+suspension.ID, nil, nil); err != nil {
		return err
	}

	// 6. Changing the calendar (archive the window, register a replacement)
	// must not rewrite the admission already frozen on the active plan.
	if err := h.call(http.MethodDelete,
		"/v1/calendars/"+cal.ID+"/windows/"+window.ID, nil, nil); err != nil {
		return err
	}
	var replacement domain.MaintenanceWindow
	if err := h.call(http.MethodPut, "/v1/calendars/"+cal.ID+"/windows", domain.WindowInput{
		Name:     "friday-evening",
		Kind:     string(domain.EntryWindow),
		Schedule: string(domain.ScheduleWeekly),
		Weekly: &domain.WeeklyInput{
			Weekday:         int(time.Friday),
			Start:           "20:00",
			DurationMinutes: 240,
			GapPolicy:       string(domain.GapShift),
		},
	}, &replacement); err != nil {
		return err
	}
	var fetched domain.RolloutPlan
	if err := h.call(http.MethodGet, "/v1/plans/"+plan.ID, nil, &fetched); err != nil {
		return err
	}
	if fetched.Admission == nil || fetched.Admission.MatchedWindowID != frozenMatchedWindow {
		return ensure(false, "calendar change silently rewrote the frozen admission")
	}

	// 7. DST determinism through the API. New York springs forward
	// 2026-03-08 02:00 -> 03:00; the 02:30 weekly anchor is in the gap.
	var nyc domain.Calendar
	if err := h.call(http.MethodPut, "/v1/calendars", domain.CalendarInput{
		Name:       "new york windows",
		Timezone:   "America/New_York",
		ScopeZones: []string{"dst.example"},
	}, &nyc); err != nil {
		return err
	}
	addNYCWindow := func(policy string) (domain.MaintenanceWindow, error) {
		var entry domain.MaintenanceWindow
		err := h.call(http.MethodPut, "/v1/calendars/"+nyc.ID+"/windows", domain.WindowInput{
			Name:     "sunday-early",
			Kind:     string(domain.EntryWindow),
			Schedule: string(domain.ScheduleWeekly),
			Weekly: &domain.WeeklyInput{
				Weekday:         int(time.Sunday),
				Start:           "02:30",
				DurationMinutes: 120,
				GapPolicy:       policy,
			},
		}, &entry)
		return entry, err
	}
	shiftWindow, err := addNYCWindow(string(domain.GapShift))
	if err != nil {
		return err
	}
	var shifted domain.AdmissionDecision
	if err := h.call(http.MethodPatch, "/v1/calendar-admission", map[string]string{
		"zone": "dst.example",
		"at":   "2026-03-08T07:00:00Z",
	}, &shifted); err != nil {
		return err
	}
	if !shifted.Allowed || shifted.MatchedWindowID != shiftWindow.ID {
		return ensure(false, "gap policy shift must move the occurrence to 03:00 local")
	}
	if err := h.call(http.MethodDelete,
		"/v1/calendars/"+nyc.ID+"/windows/"+shiftWindow.ID, nil, nil); err != nil {
		return err
	}
	rejectWindow, err := addNYCWindow(string(domain.GapReject))
	if err != nil {
		return err
	}
	var rejected domain.AdmissionDecision
	if err := h.call(http.MethodPatch, "/v1/calendar-admission", map[string]string{
		"zone": "dst.example",
		"at":   "2026-03-08T07:30:00Z",
	}, &rejected); err != nil {
		return err
	}
	if rejected.Allowed || rejected.MatchedWindowID == rejectWindow.ID {
		return ensure(false, "gap policy reject must skip the occurrence")
	}

	// 8. Cross-midnight weekly window: Friday 23:30 for 60 minutes covers
	// Saturday 00:00-00:30 UTC and ends deterministically at 00:30.
	var midnightCal domain.Calendar
	if err := h.call(http.MethodPut, "/v1/calendars", domain.CalendarInput{
		Name:       "cross midnight calendar",
		Timezone:   "UTC",
		ScopeZones: []string{"midnight.example"},
	}, &midnightCal); err != nil {
		return err
	}
	var midnightWindow domain.MaintenanceWindow
	if err := h.call(http.MethodPut, "/v1/calendars/"+midnightCal.ID+"/windows", domain.WindowInput{
		Name:     "friday-late",
		Kind:     string(domain.EntryWindow),
		Schedule: string(domain.ScheduleWeekly),
		Weekly: &domain.WeeklyInput{
			Weekday:         int(time.Friday),
			Start:           "23:30",
			DurationMinutes: 60,
			GapPolicy:       string(domain.GapShift),
		},
	}, &midnightWindow); err != nil {
		return err
	}
	for _, instant := range []string{"2026-09-25T23:45:00Z", "2026-09-26T00:15:00Z"} {
		var decision domain.AdmissionDecision
		if err := h.call(http.MethodPatch, "/v1/calendar-admission", map[string]string{
			"zone": "midnight.example",
			"at":   instant,
		}, &decision); err != nil {
			return err
		}
		if !decision.Allowed || decision.MatchedWindowID != midnightWindow.ID {
			return ensure(false, "cross-midnight occurrence must cover "+instant)
		}
	}
	var afterClose domain.AdmissionDecision
	if err := h.call(http.MethodPatch, "/v1/calendar-admission", map[string]string{
		"zone": "midnight.example",
		"at":   "2026-09-26T00:45:00Z",
	}, &afterClose); err != nil {
		return err
	}
	if afterClose.Allowed {
		return ensure(false, "cross-midnight occurrence must end at 00:30 sharp")
	}

	// 9. Fall-back ambiguity: New York repeats 01:00-02:00 on 2026-11-01;
	// the engine deterministically chooses the earlier instant, so a
	// 30-minute window anchored at 01:30 covers 05:30Z but not 06:30Z.
	var foldCal domain.Calendar
	if err := h.call(http.MethodPut, "/v1/calendars", domain.CalendarInput{
		Name:       "fold calendar",
		Timezone:   "America/New_York",
		ScopeZones: []string{"fold.example"},
	}, &foldCal); err != nil {
		return err
	}
	var foldWindow domain.MaintenanceWindow
	if err := h.call(http.MethodPut, "/v1/calendars/"+foldCal.ID+"/windows", domain.WindowInput{
		Name:     "sunday-fold",
		Kind:     string(domain.EntryWindow),
		Schedule: string(domain.ScheduleWeekly),
		Weekly: &domain.WeeklyInput{
			Weekday:         int(time.Sunday),
			Start:           "01:30",
			DurationMinutes: 30,
			GapPolicy:       string(domain.GapShift),
		},
	}, &foldWindow); err != nil {
		return err
	}
	var earlier domain.AdmissionDecision
	if err := h.call(http.MethodPatch, "/v1/calendar-admission", map[string]string{
		"zone": "fold.example",
		"at":   "2026-11-01T05:45:00Z",
	}, &earlier); err != nil {
		return err
	}
	if !earlier.Allowed || earlier.MatchedWindowID != foldWindow.ID {
		return ensure(false, "fold must deterministically map to the earlier instant at 05:30Z")
	}
	var later domain.AdmissionDecision
	if err := h.call(http.MethodPatch, "/v1/calendar-admission", map[string]string{
		"zone": "fold.example",
		"at":   "2026-11-01T06:45:00Z",
	}, &later); err != nil {
		return err
	}
	if later.Allowed {
		return ensure(false, "fold must not cover the second 01:30 at 06:30Z")
	}

	// 10. Restart: reopen the persisted snapshot and confirm the calendars
	// and windows are restored and evaluate identically.
	reopened, err := store.Open(h.tempDir + "/state.json")
	if err != nil {
		return err
	}
	defer func() { _ = reopened }()
	calendars := calendar.New(reopened, h.clock, platform.CryptoIDGenerator{})
	decision, err := calendars.Resolve(h.ctx(), scheduleScope("windowed.example"),
		parseRFC3339("2026-09-25T21:00:00Z"))
	if err != nil {
		return err
	}
	if !decision.Allowed || decision.MatchedWindowID != replacement.ID {
		return ensure(false, "restart did not restore the current windows")
	}
	decision, err = calendars.Resolve(h.ctx(), scheduleScope("windowed.example"),
		parseRFC3339("2026-09-25T12:00:00Z"))
	if err != nil {
		return err
	}
	if decision.Allowed {
		return ensure(false, "restart restored an archived window that must be gone")
	}
	return nil
}

func addRevision(h *harness, planID string, serial uint32) (domain.ZoneRevision, error) {
	var revision domain.ZoneRevision
	err := h.call(http.MethodPatch, "/v1/plans/"+planID+"/revisions", domain.RevisionCreateInput{
		Serial:    serial,
		Authority: "ns1.windowed.example",
		Entries: []domain.ZoneEntry{
			{Owner: "windowed.example", Type: domain.EntryA, Value: "192.0.2.20"},
		},
	}, &revision)
	return revision, err
}

func createDraftPlanWithRevision(h *harness, zone string, serial uint32) (domain.RolloutPlan, domain.ZoneRevision, error) {
	var plan domain.RolloutPlan
	if err := h.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone: zone, MinimumSuccessRatio: 1, MaximumLatencyMS: 80, ExpectedNodes: 1,
	}, &plan); err != nil {
		return domain.RolloutPlan{}, domain.ZoneRevision{}, err
	}
	revision, err := addRevision(h, plan.ID, serial)
	if err != nil {
		return domain.RolloutPlan{}, domain.ZoneRevision{}, err
	}
	plan.Version = 2
	return plan, revision, nil
}

func parseRFC3339(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}
