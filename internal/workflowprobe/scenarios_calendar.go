package workflowprobe

import (
	"net/http"
	"net/url"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
)

// runCalendarWindowGating exercises the timezone-aware rolling calendar through
// the real HTTP surface: one-time and weekly windows, pauses, region scoping,
// cross-midnight and DST occurrences, deterministic overlap resolution, frozen
// approval interpretations, default-all-hours plans, and snapshot restore.
func runCalendarWindowGating() error {
	harness, err := newHarness()
	if err != nil {
		return err
	}
	defer harness.close()

	if err := checkUnconfiguredAndDeletion(harness); err != nil {
		return err
	}
	if err := checkCrossMidnightAndOverlap(harness); err != nil {
		return err
	}
	if err := checkDaylightSavingAndPauses(harness); err != nil {
		return err
	}
	if err := checkScopingFreezeAndRestart(harness); err != nil {
		return err
	}
	if err := checkWindowValidation(harness); err != nil {
		return err
	}
	return nil
}

func checkUnconfiguredAndDeletion(harness *harness) error {
	plan := mustDraftPlan(harness, "free.example")

	// A plan without a calendar is deployable at all hours.
	decision := mustEvaluate(harness, plan.ID, "", time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	if err := ensure(decision.Allowed && decision.Mode == "all_hours" &&
		decision.Reason == domain.WindowReasonUnconfigured, "unconfigured plan was not all hours"); err != nil {
		return err
	}

	body := map[string]any{
		"mode": "permit", "schedule": "once", "timezone": "UTC",
		"starts_at": "2026-09-25T18:00:00Z",
		"ends_at":   "2026-09-25T19:00:00Z",
	}
	var window domain.WindowEntry
	if err := harness.call(http.MethodPut, "/v1/plans/"+plan.ID+"/calendar/windows", body, &window); err != nil {
		return err
	}
	decision = mustEvaluate(harness, plan.ID, "", time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	if err := ensure(!decision.Allowed && decision.Reason == domain.WindowReasonNoPermit,
		"configured calendar did not default-deny outside windows"); err != nil {
		return err
	}
	if decision.NextOpeningAt == nil || !decision.NextOpeningAt.Equal(time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)) {
		return ensure(false, "next opening was not reported")
	}

	if err := harness.call(http.MethodDelete, "/v1/plans/"+plan.ID+"/calendar/windows/"+window.ID, nil, nil); err != nil {
		return err
	}
	decision = mustEvaluate(harness, plan.ID, "", time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	if err := ensure(decision.Allowed && decision.Mode == "all_hours",
		"deleting every window did not restore all-hours rollout"); err != nil {
		return err
	}
	return nil
}

func checkCrossMidnightAndOverlap(harness *harness) error {
	plan := mustDraftPlan(harness, "night.example")

	// Friday 23:00 to 01:00 Asia/Shanghai spills across midnight. The concrete
	// 2026-09-25 occurrence is 15:00Z through 17:00Z.
	weekly := mustRegister(harness, plan.ID, map[string]any{
		"mode": "permit", "schedule": "weekly", "timezone": "Asia/Shanghai",
		"weekdays": []int{5}, "start_time": "23:00", "end_time": "01:00",
	})

	at := func(hour, minute int) time.Time {
		return time.Date(2026, 9, 25, hour, minute, 0, 0, time.UTC)
	}
	decision := mustEvaluate(harness, plan.ID, "", at(14, 59))
	if err := ensure(!decision.Allowed, "cross-midnight window opened before its start"); err != nil {
		return err
	}
	decision = mustEvaluate(harness, plan.ID, "", at(15, 30))
	if err := ensure(decision.Allowed && decision.Basis != nil &&
		decision.Basis.WindowID == weekly.ID &&
		decision.Basis.StartsAt.Equal(at(15, 0)) &&
		decision.Basis.EndsAt.Equal(at(17, 0)),
		"cross-midnight occurrence was not interpreted in Shanghai time"); err != nil {
		return err
	}
	if err := ensure(decision.Basis.LocalTime == "2026-09-25T23:30:00+08:00",
		"basis local time did not match the configured timezone"); err != nil {
		return err
	}
	// Empty regions apply to every region scope as well.
	regionDecision := mustEvaluate(harness, plan.ID, "west", at(15, 30))
	if err := ensure(regionDecision.Allowed, "plan-wide window did not apply to a region"); err != nil {
		return err
	}
	decision = mustEvaluate(harness, plan.ID, "", at(17, 0))
	if err := ensure(!decision.Allowed, "window end instant was not excluded"); err != nil {
		return err
	}

	// Overlapping permits resolve deterministically: the occurrence with the
	// earliest concrete start is the basis regardless of registration order.
	oneOff := mustRegister(harness, plan.ID, map[string]any{
		"mode": "permit", "schedule": "once", "timezone": "UTC",
		"starts_at": "2026-09-25T15:30:00Z",
		"ends_at":   "2026-09-25T16:30:00Z",
	})
	decision = mustEvaluate(harness, plan.ID, "", at(16, 0))
	if err := ensure(decision.Basis != nil && decision.Basis.WindowID == weekly.ID,
		"overlap basis was not the earliest-starting occurrence"); err != nil {
		return err
	}
	decision = mustEvaluate(harness, plan.ID, "", at(16, 15))
	if err := ensure(decision.Basis.WindowID == weekly.ID && oneOff.ID != weekly.ID,
		"overlap basis changed inside the overlap"); err != nil {
		return err
	}
	return nil
}

func checkDaylightSavingAndPauses(harness *harness) error {
	// Fall-back occurrence on 2026-11-01: 01:00->03:00 America/New_York maps
	// deterministically to 05:00Z->07:00Z and covers the repeated hour once.
	fallback := mustDraftPlan(harness, "fall.example")
	mustRegister(harness, fallback.ID, map[string]any{
		"mode": "permit", "schedule": "weekly", "timezone": "America/New_York",
		"weekdays": []int{0}, "start_time": "01:00", "end_time": "03:00",
	})
	z := func(value string) time.Time {
		parsed, _ := time.Parse(time.RFC3339, value)
		return parsed
	}
	decision := mustEvaluate(harness, fallback.ID, "", z("2026-11-01T04:30:00Z"))
	if err := ensure(!decision.Allowed && decision.NextOpeningAt != nil &&
		decision.NextOpeningAt.Equal(z("2026-11-01T05:00:00Z")),
		"next opening ignored the fall-back DST transition"); err != nil {
		return err
	}
	decision = mustEvaluate(harness, fallback.ID, "", z("2026-11-01T06:30:00Z"))
	if err := ensure(decision.Allowed && decision.Basis.StartsAt.Equal(z("2026-11-01T05:00:00Z")) &&
		decision.Basis.EndsAt.Equal(z("2026-11-01T07:00:00Z")),
		"repeated hour during fall-back was not covered by one occurrence"); err != nil {
		return err
	}

	// A pause overrides overlapping permits and reports the next uncovered
	// instant when it ends.
	pause := mustRegister(harness, fallback.ID, map[string]any{
		"mode": "pause", "schedule": "once", "timezone": "UTC",
		"starts_at": "2026-11-01T05:30:00Z",
		"ends_at":   "2026-11-01T06:30:00Z",
	})
	decision = mustEvaluate(harness, fallback.ID, "", z("2026-11-01T06:00:00Z"))
	if err := ensure(!decision.Allowed && decision.Reason == domain.WindowReasonPaused &&
		decision.BlockingPauseID == pause.ID &&
		decision.NextOpeningAt != nil && decision.NextOpeningAt.Equal(z("2026-11-01T06:30:00Z")),
		"pause did not override the permit or report the trimmed reopening"); err != nil {
		return err
	}
	decision = mustEvaluate(harness, fallback.ID, "", z("2026-11-01T06:30:00Z"))
	if err := ensure(decision.Allowed, "pause end instant was not reopened"); err != nil {
		return err
	}

	// Spring-forward gap on 2026-03-08: nominal 02:30 does not exist in New
	// York. Go deterministically treats wall times after 2 a.m. as EDT, so
	// 02:30 becomes 02:30 EDT and the occurrence is 06:30Z->07:30Z.
	spring := mustDraftPlan(harness, "spring.example")
	mustRegister(harness, spring.ID, map[string]any{
		"mode": "permit", "schedule": "weekly", "timezone": "America/New_York",
		"weekdays": []int{0}, "start_time": "02:30", "end_time": "03:30",
	})
	decision = mustEvaluate(harness, spring.ID, "", z("2026-03-08T06:00:00Z"))
	if err := ensure(!decision.Allowed, "time before the normalized spring occurrence was treated as open"); err != nil {
		return err
	}
	decision = mustEvaluate(harness, spring.ID, "", z("2026-03-08T06:30:00Z"))
	if err := ensure(decision.Allowed && decision.Basis.StartsAt.Equal(z("2026-03-08T06:30:00Z")) &&
		decision.Basis.EndsAt.Equal(z("2026-03-08T07:30:00Z")),
		"spring-forward gap was not normalized deterministically"); err != nil {
		return err
	}
	decision = mustEvaluate(harness, spring.ID, "", z("2026-03-08T07:00:00Z"))
	if err := ensure(decision.Allowed, "normalized spring occurrence did not stay open through 03:00 EDT"); err != nil {
		return err
	}
	decision = mustEvaluate(harness, spring.ID, "", z("2026-03-08T07:30:00Z"))
	if err := ensure(!decision.Allowed, "spring occurrence end was not excluded"); err != nil {
		return err
	}
	return nil
}

func checkScopingFreezeAndRestart(harness *harness) error {
	plan := mustDraftPlan(harness, "gate.example")
	if _, err := enrollNode(harness, "resolver-east-9", "east"); err != nil {
		return err
	}
	if _, err := enrollNode(harness, "resolver-west-9", "west"); err != nil {
		return err
	}

	// West-only weekly window: Friday 19:00-21:00 Shanghai = 11:00Z-13:00Z.
	// It must never authorize plan-wide activation, only west leases.
	mustRegister(harness, plan.ID, map[string]any{
		"mode": "permit", "schedule": "weekly", "timezone": "Asia/Shanghai",
		"weekdays": []int{5}, "start_time": "19:00", "end_time": "21:00",
		"regions": []string{"west"},
	})
	decision := mustEvaluate(harness, plan.ID, "", time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	if err := ensure(!decision.Allowed, "region-scoped window incorrectly authorized plan activation"); err != nil {
		return err
	}
	decision = mustEvaluate(harness, plan.ID, "east", time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	if err := ensure(!decision.Allowed, "west-only window incorrectly applied to east"); err != nil {
		return err
	}
	decision = mustEvaluate(harness, plan.ID, "west", time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	if err := ensure(decision.Allowed, "west window did not apply inside its region"); err != nil {
		return err
	}

	planWide := mustRegister(harness, plan.ID, map[string]any{
		"mode": "permit", "schedule": "once", "timezone": "UTC",
		"starts_at": "2026-09-25T11:00:00Z",
		"ends_at":   "2026-09-25T14:00:00Z",
	})

	// Activation and lease allocation share the same clock and evaluator.
	var activated domain.RolloutPlan
	if err := harness.call(http.MethodPatch, "/v1/plans/"+plan.ID+"/activate",
		map[string]any{"expected_version": plan.Version + 2}, &activated); err != nil {
		return err
	}
	if err := ensure(activated.WindowDecision != nil && activated.WindowDecision.Allowed &&
		activated.WindowDecision.Basis != nil &&
		activated.WindowDecision.Basis.WindowID == planWide.ID,
		"activation did not freeze its window interpretation"); err != nil {
		return err
	}

	var firstGroup domain.LeaseGroup
	if err := harness.call(http.MethodPatch, "/v1/plans/"+plan.ID+"/leases", domain.LeaseInput{
		RequestKey: "lease-east-0001", Region: "east", LeaseSeconds: 600, MaxNodes: 2,
	}, &firstGroup); err != nil {
		return err
	}
	if err := ensure(len(firstGroup.LeaseIDs) == 1 && firstGroup.WindowDecision != nil &&
		firstGroup.WindowDecision.Allowed, "in-window allocation did not carry the decision"); err != nil {
		return err
	}

	// Add a pause, move the clock into it, and confirm new allocation is
	// blocked while the idempotent replay keeps its original frozen approval.
	mustRegister(harness, plan.ID, map[string]any{
		"mode": "pause", "schedule": "once", "timezone": "UTC",
		"starts_at": "2026-09-25T12:05:00Z",
		"ends_at":   "2026-09-25T12:20:00Z",
	})
	harness.clock.Advance(10 * time.Minute)

	var replay domain.LeaseGroup
	if err := harness.call(http.MethodPatch, "/v1/plans/"+plan.ID+"/leases", domain.LeaseInput{
		RequestKey: "lease-east-0001", Region: "east", LeaseSeconds: 600, MaxNodes: 2,
	}, &replay); err != nil {
		return err
	}
	if err := ensure(replay.ID == firstGroup.ID && replay.WindowDecision.EvaluatedAt.Equal(firstGroup.WindowDecision.EvaluatedAt),
		"calendar change rewrote an already granted lease interpretation"); err != nil {
		return err
	}

	err := harness.call(http.MethodPatch, "/v1/plans/"+plan.ID+"/leases", domain.LeaseInput{
		RequestKey: "lease-west-0001", Region: "west", LeaseSeconds: 600, MaxNodes: 2,
	}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeWindowClosed)); err != nil {
		return err
	}

	// Past the pause, west allocation succeeds and must not have leaked partial
	// leases from the denied attempt.
	harness.clock.Advance(15 * time.Minute)
	var westGroup domain.LeaseGroup
	if err := harness.call(http.MethodPatch, "/v1/plans/"+plan.ID+"/leases", domain.LeaseInput{
		RequestKey: "lease-west-0002", Region: "west", LeaseSeconds: 600, MaxNodes: 2,
	}, &westGroup); err != nil {
		return err
	}
	if err := ensure(len(westGroup.LeaseIDs) == 1, "denied allocation left partial lease state"); err != nil {
		return err
	}

	// Replace the plan-wide permit with one that no longer covers the present.
	// New east allocation must deny while frozen approvals stay untouched.
	mustReplace(harness, plan.ID, planWide.ID, map[string]any{
		"mode": "permit", "schedule": "once", "timezone": "UTC",
		"starts_at": "2026-09-25T02:00:00Z",
		"ends_at":   "2026-09-25T03:00:00Z",
	})
	var refetched domain.RolloutPlan
	if err := harness.call(http.MethodGet, "/v1/plans/"+plan.ID, nil, &refetched); err != nil {
		return err
	}
	if err := ensure(refetched.WindowDecision != nil && refetched.WindowDecision.Basis != nil &&
		refetched.WindowDecision.Basis.WindowID == planWide.ID &&
		refetched.WindowDecision.EvaluatedAt.Equal(activated.WindowDecision.EvaluatedAt),
		"window replacement rewrote the plan's activation interpretation"); err != nil {
		return err
	}
	err = harness.call(http.MethodPatch, "/v1/plans/"+plan.ID+"/leases", domain.LeaseInput{
		RequestKey: "lease-east-0002", Region: "east", LeaseSeconds: 600, MaxNodes: 2,
	}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeWindowClosed)); err != nil {
		return err
	}
	if err := harness.call(http.MethodPatch, "/v1/plans/"+plan.ID+"/leases", domain.LeaseInput{
		RequestKey: "lease-east-0001", Region: "east", LeaseSeconds: 600, MaxNodes: 2,
	}, &replay); err != nil {
		return err
	}
	if err := ensure(replay.ID == firstGroup.ID, "frozen lease group was lost after calendar edits"); err != nil {
		return err
	}

	// Restart: the snapshot must restore the calendar and every frozen decision.
	if err := harness.relaunch(); err != nil {
		return err
	}
	if err := harness.call(http.MethodGet, "/v1/plans/"+plan.ID, nil, &refetched); err != nil {
		return err
	}
	if err := ensure(refetched.WindowDecision != nil &&
		refetched.WindowDecision.Basis.WindowID == planWide.ID,
		"activation interpretation did not survive restart"); err != nil {
		return err
	}
	var calendarView struct {
		Windows []domain.WindowEntry `json:"windows"`
	}
	if err := harness.call(http.MethodGet, "/v1/plans/"+plan.ID+"/calendar", nil, &calendarView); err != nil {
		return err
	}
	if err := ensure(len(calendarView.Windows) == 3, "calendar was not restored after restart"); err != nil {
		return err
	}
	if err := harness.call(http.MethodPatch, "/v1/plans/"+plan.ID+"/leases", domain.LeaseInput{
		RequestKey: "lease-east-0001", Region: "east", LeaseSeconds: 600, MaxNodes: 2,
	}, &replay); err != nil {
		return err
	}
	return ensure(replay.ID == firstGroup.ID, "lease group did not replay after restart")
}

func checkWindowValidation(harness *harness) error {
	plan := mustDraftPlan(harness, "invalid.example")
	invalidBodies := []map[string]any{
		{"mode": "maybe", "schedule": "once", "timezone": "UTC",
			"starts_at": "2026-09-25T18:00:00Z", "ends_at": "2026-09-25T19:00:00Z"},
		{"mode": "permit", "schedule": "weekly", "timezone": "Not/AZone",
			"weekdays": []int{1}, "start_time": "10:00", "end_time": "11:00"},
		{"mode": "permit", "schedule": "weekly", "timezone": "UTC",
			"weekdays": []int{}, "start_time": "10:00", "end_time": "11:00"},
		{"mode": "permit", "schedule": "once", "timezone": "UTC",
			"starts_at": "2026-09-25T19:00:00Z"},
		{"mode": "permit", "schedule": "weekly", "timezone": "UTC",
			"weekdays": []int{1}, "start_time": "10:00", "end_time": "10:00"},
	}
	for _, body := range invalidBodies {
		err := harness.call(http.MethodPut, "/v1/plans/"+plan.ID+"/calendar/windows", body, nil)
		if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
			return err
		}
	}
	return nil
}

func mustDraftPlan(harness *harness, zone string) domain.RolloutPlan {
	var plan domain.RolloutPlan
	if err := harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone: zone, MinimumSuccessRatio: 1, MaximumLatencyMS: 80, ExpectedNodes: 1,
	}, &plan); err != nil {
		panic(err)
	}
	if err := harness.call(http.MethodPatch, "/v1/plans/"+plan.ID+"/revisions", domain.RevisionCreateInput{
		Serial:    2026092501,
		Authority: "ns1." + zone,
		Entries:   []domain.ZoneEntry{{Owner: zone, Type: domain.EntryA, Value: "192.0.2.10"}},
	}, nil); err != nil {
		panic(err)
	}
	plan.Version++
	return plan
}

func mustRegister(harness *harness, planID string, body map[string]any) domain.WindowEntry {
	var entry domain.WindowEntry
	if err := harness.call(http.MethodPut, "/v1/plans/"+planID+"/calendar/windows", body, &entry); err != nil {
		panic(err)
	}
	return entry
}

func mustReplace(harness *harness, planID string, windowID string, body map[string]any) domain.WindowEntry {
	var entry domain.WindowEntry
	if err := harness.call(http.MethodPut,
		"/v1/plans/"+planID+"/calendar/windows/"+windowID, body, &entry); err != nil {
		panic(err)
	}
	return entry
}

func mustEvaluate(harness *harness, planID string, region string, at time.Time) domain.WindowDecision {
	query := url.Values{}
	query.Set("at", at.Format(time.RFC3339))
	if region != "" {
		query.Set("region", region)
	}
	var decision domain.WindowDecision
	if err := harness.call(http.MethodGet,
		"/v1/plans/"+planID+"/calendar/evaluation?"+query.Encode(), nil, &decision); err != nil {
		panic(err)
	}
	return decision
}
