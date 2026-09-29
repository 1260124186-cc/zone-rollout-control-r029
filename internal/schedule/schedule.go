// Package schedule contains the pure, deterministic calendar evaluation
// engine. It has no dependency on the store, HTTP layer, or filesystem, so
// plan activation and lease allocation interpret maintenance time through
// exactly the same code path.
package schedule

import (
	"sort"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
)

// Interval is a half-open UTC interval [Start, End).
type Interval struct {
	Start time.Time
	End   time.Time
}

// covers reports whether the instant falls inside the half-open interval.
func (interval Interval) covers(instant time.Time) bool {
	return !instant.Before(interval.Start) && instant.Before(interval.End)
}

// transition marks one offset change observed around an anchor day.
type transition struct {
	at     time.Time // first instant at which the new offset applies
	before int       // offset (seconds east of UTC) just before
	after  int       // offset from at onward
}

// dayTransitions scans 26 hours on either side of local midnight in
// one-minute absolute steps for UTC offset changes. Real tzdata transitions
// are aligned to whole minutes and a calendar day spans 23-25 absolute
// hours; the wide margin also covers transitions exactly at midnight, where
// time.Date normalizes the missing midnight anchor forward across the gap.
func dayTransitions(location *time.Location, year int, month time.Month, day int) []transition {
	base := time.Date(year, month, day, 0, 0, 0, 0, location)
	found := make([]transition, 0, 2)
	margin := 26 * time.Hour
	previous := base.Add(-margin)
	previousOffset := zoneOffset(location, previous)
	for step := 1; step <= int((2*margin)/time.Minute); step++ {
		current := base.Add(-margin + time.Duration(step)*time.Minute)
		currentOffset := zoneOffset(location, current)
		if currentOffset != previousOffset {
			found = append(found, transition{at: current, before: previousOffset, after: currentOffset})
		}
		previous = current
		previousOffset = currentOffset
	}
	return found
}

func zoneOffset(location *time.Location, instant time.Time) int {
	_, value := instant.In(location).Zone()
	return value
}

func wallInstant(year int, month time.Month, day, hour, minute, offsetSeconds int) time.Time {
	wallClock := time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
	return wallClock.Add(-time.Duration(offsetSeconds) * time.Second)
}

// ResolveLocal maps a local wall clock time inside location to a UTC instant.
//
// Deterministic daylight saving rules:
//   - A normal wall clock maps to its unique instant.
//   - A wall clock in a spring-forward gap never occurs; shiftForward=true
//     returns the first instant after the gap (clocks jump forward to it),
//     while false reports ok=false so the caller can skip that occurrence
//     (the "reject" policy).
//   - An ambiguous fall-back wall clock maps deterministically to the
//     earlier (pre-transition) instant.
//
// Classification counts how many of the day's UTC offsets map an absolute
// instant back to the requested wall clock: one means unique, two means a
// fall-back fold, zero means a spring-forward gap. Working with instants
// instead of minute-of-day ranges keeps transitions at midnight
// deterministic as well.
func ResolveLocal(location *time.Location, year int, month time.Month, day, hour, minute int, shiftForward bool) (time.Time, bool) {
	transitions := dayTransitions(location, year, month, day)
	offsets := map[int]struct{}{
		zoneOffset(location, time.Date(year, month, day, 12, 0, 0, 0, location)): {},
	}
	for _, change := range transitions {
		offsets[change.before] = struct{}{}
		offsets[change.after] = struct{}{}
	}
	valid := make([]time.Time, 0, 2)
	for offsetSeconds := range offsets {
		candidate := wallInstant(year, month, day, hour, minute, offsetSeconds)
		if sameWall(candidate.In(location), year, month, day, hour, minute) {
			valid = append(valid, candidate)
		}
	}
	switch len(valid) {
	case 1:
		return valid[0].UTC(), true
	case 2:
		// Ambiguous fall-back: choose the earlier (pre-transition) instant.
		earlier := valid[0]
		if valid[1].Before(earlier) {
			earlier = valid[1]
		}
		return earlier.UTC(), true
	default:
		// Spring-forward gap. Locate the transition that skips this wall
		// clock; shifting lands at the first post-gap instant.
		for _, change := range transitions {
			if change.after <= change.before {
				continue
			}
			asBefore := wallInstant(year, month, day, hour, minute, change.before)
			asAfter := wallInstant(year, month, day, hour, minute, change.after)
			if !asBefore.Before(change.at) && asAfter.Before(change.at) {
				if shiftForward {
					return change.at.UTC(), true
				}
				return time.Time{}, false
			}
		}
		// Defensive fallback for unexpected tzdata shapes.
		if shiftForward {
			return time.Date(year, month, day, hour, minute, 0, 0, location).UTC(), true
		}
		return time.Time{}, false
	}
}

func sameWall(instant time.Time, year int, month time.Month, day, hour, minute int) bool {
	return instant.Year() == year &&
		instant.Month() == month &&
		instant.Day() == day &&
		instant.Hour() == hour &&
		instant.Minute() == minute
}

// ExpandWeekly builds concrete UTC intervals for a weekly entry using the
// owning calendar's timezone. The interval may cross midnight: it ends
// exactly DurationMinutes after the resolved start instant, so a DST shift
// never changes which instant is the end. Two days on either side of the
// anchor date cover every maximum-length (24h) occurrence.
func ExpandWeekly(entry domain.MaintenanceWindow, timezone string, instant time.Time) ([]Interval, error) {
	if entry.Weekly == nil {
		return nil, nil
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, err
	}
	local := instant.In(location)
	startMinute := entry.Weekly.StartMinute
	hour := startMinute / 60
	minute := startMinute % 60
	duration := time.Duration(entry.Weekly.DurationMinutes) * time.Minute
	intervals := make([]Interval, 0, 3)
	for offsetDays := -2; offsetDays <= 2; offsetDays++ {
		candidateDay := local.AddDate(0, 0, offsetDays)
		if candidateDay.Weekday() != entry.Weekly.Weekday {
			continue
		}
		start, ok := ResolveLocal(
			location,
			candidateDay.Year(),
			candidateDay.Month(),
			candidateDay.Day(),
			hour,
			minute,
			entry.Weekly.GapPolicy == domain.GapShift,
		)
		if !ok {
			// Gap policy "reject": this occurrence is skipped entirely.
			continue
		}
		intervals = append(intervals, Interval{Start: start, End: start.Add(duration)})
	}
	sort.Slice(intervals, func(i, j int) bool {
		return intervals[i].Start.Before(intervals[j].Start)
	})
	return intervals, nil
}

// OneTimeInterval converts an absolute one-time entry to a UTC interval.
func OneTimeInterval(entry domain.MaintenanceWindow) (Interval, bool) {
	if entry.OneTime == nil {
		return Interval{}, false
	}
	return Interval{Start: entry.OneTime.StartAt.UTC(), End: entry.OneTime.EndAt.UTC()}, true
}

// EntryIntervals expands every concrete interval of an entry around instant.
func EntryIntervals(entry domain.MaintenanceWindow, timezone string, instant time.Time) ([]Interval, error) {
	switch entry.Schedule {
	case domain.ScheduleOneTime:
		interval, ok := OneTimeInterval(entry)
		if !ok {
			return nil, nil
		}
		return []Interval{interval}, nil
	case domain.ScheduleWeekly:
		return ExpandWeekly(entry, timezone, instant)
	default:
		return nil, nil
	}
}
