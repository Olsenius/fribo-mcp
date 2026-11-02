package main

import (
	"strings"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, oslo)
	if err != nil {
		panic(err)
	}
	return t
}

// testConfig mirrors a real contract: order by 11:00 the day before on work
// days, two days before on weekends, first plowing at 05:00.
func testConfig() *PlowingConfig {
	cfg := &PlowingConfig{
		StartSeasonDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		EndSeasonDate:   time.Date(2027, 6, 30, 0, 0, 0, 0, time.UTC),
	}
	first := time.Date(2026, 11, 2, 4, 0, 0, 0, time.UTC)
	cfg.FirstHourPlowing = &first
	cutoff := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC) // 11:00 in Oslo
	evening := time.Date(2026, 11, 2, 22, 0, 0, 0, time.UTC)
	cfg.PlowingSchedule.WorkDaysDeadline = Deadline{ArrivalDeadline: 1, ArrivalDeadlineHours: cutoff, DepartureDeadlineHours: evening}
	cfg.PlowingSchedule.WeekendsDeadline = Deadline{ArrivalDeadline: 2, ArrivalDeadlineHours: cutoff, DepartureDeadlineHours: evening}
	return cfg
}

func TestBuildPlanUsesNorwegianTime(t *testing.T) {
	p, err := buildPlan("2026-12-18", 13, "2026-12-20", []string{"2026-12-19", "2026-12-19"})
	if err != nil {
		t.Fatal(err)
	}
	if got := isoMillis(p.Arrival); got != "2026-12-18T12:00:00.000Z" {
		t.Fatalf("winter arrival %s", got)
	}
	if got := isoMillis(p.Departure); got != "2026-12-20T22:59:59.000Z" {
		t.Fatalf("winter departure %s", got)
	}
	if got := p.skipDays(); len(got) != 1 || got[0] != "2026-12-19" {
		t.Fatalf("skip days %v", got)
	}
	p, err = buildPlan("2027-04-04", 8, "2027-04-06", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := isoMillis(p.Arrival); got != "2027-04-04T06:00:00.000Z" {
		t.Fatalf("summer-time arrival %s", got)
	}
	if got := p.skipDays(); got == nil || len(got) != 0 {
		t.Fatalf("want an empty list, got %#v", got)
	}
}

func TestBuildPlanRejectsBadInput(t *testing.T) {
	for _, c := range []struct {
		arr, dep string
		hour     int
		skip     []string
		want     string
	}{
		{"18.12.2026", "2026-12-20", 13, nil, "arrival_date"},
		{"2026-12-18", "2026-12-17", 13, nil, "before arrival"},
		{"2026-12-18", "2026-12-20", 24, nil, "arrival_hour"},
		{"2026-12-18", "2026-12-20", 13, []string{"2026-12-21"}, "outside the stay"},
	} {
		_, err := buildPlan(c.arr, c.hour, c.dep, c.skip)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: got %v", c, err)
		}
	}
}

func TestEarliestDayFollowsCutoff(t *testing.T) {
	cfg := testConfig()
	tuesday := at("2026-12-15 00:00")
	for _, c := range []struct{ now, want string }{
		{"2026-12-14 10:59", "2026-12-15"}, // Monday before 11: tomorrow is fine
		{"2026-12-14 11:00", "2026-12-16"}, // from 11 it is the day after
	} {
		if got := earliestDay(tuesday, "arrival", cfg, at(c.now)).Format(dateLayout); got != c.want {
			t.Errorf("now %s: earliest %s, want %s", c.now, got, c.want)
		}
	}
	// Saturday uses the weekend deadline of two days.
	if got := earliestDay(at("2026-12-19 00:00"), "arrival", cfg, at("2026-12-14 09:00")).Format(dateLayout); got != "2026-12-16" {
		t.Errorf("weekend earliest %s", got)
	}
	// Exception days count as weekends.
	cfg.PlowingSchedule.WorkingExceptionDays = []DateRange{{StartDate: at("2026-12-15 00:00"), EndDate: at("2026-12-15 00:00")}}
	if !weekendLike(tuesday, cfg) {
		t.Error("exception day not treated as weekend")
	}
}

func TestEarliestGuaranteedArrivalSkipsWeekendGap(t *testing.T) {
	cfg := testConfig()
	// Thursday 12:00, after the cut-off: Friday to Sunday need two days plus
	// one, so Sunday is the first day that can still be ordered in time.
	if got := earliestGuaranteedArrival(cfg, at("2026-12-17 12:00")); got != "2026-12-20" {
		t.Fatalf("earliest %s", got)
	}
}

func TestAssess(t *testing.T) {
	cfg := testConfig()
	now := at("2026-12-14 12:00")

	p, _ := buildPlan("2026-12-17", 13, "2026-12-18", nil)
	if a := assess(p, true, cfg, now); !a.PlowingGuaranteed || len(a.Problems) > 0 || a.ArrivalDeadline != "by 11:00 the day before" {
		t.Fatalf("in time: %+v", a)
	}

	p, _ = buildPlan("2026-12-15", 13, "2026-12-16", nil)
	a := assess(p, true, cfg, now)
	if a.PlowingGuaranteed || len(a.Problems) > 0 || len(a.Notes) == 0 {
		t.Fatalf("late but allowed: %+v", a)
	}

	p, _ = buildPlan("2026-12-17", 4, "2026-12-18", nil)
	if a := assess(p, true, cfg, now); a.PlowingGuaranteed || !strings.Contains(strings.Join(a.Problems, ";"), "first plowing hour") {
		t.Fatalf("before first hour: %+v", a)
	}

	p, _ = buildPlan("2027-07-02", 13, "2027-07-04", nil)
	// Fribo accepts stays outside the season in its settings, so this is only a note.
	if a := assess(p, true, cfg, now); !a.PlowingGuaranteed || len(a.Problems) > 0 || !strings.Contains(strings.Join(a.Notes, ";"), "season") {
		t.Fatalf("out of season: %+v", a)
	}

	// Changing only the departure checks the departure deadline.
	p, _ = buildPlan("2026-12-12", 13, "2026-12-13", nil)
	if a := assess(p, false, cfg, now); !strings.Contains(strings.Join(a.Problems, ";"), "departure is in the past") {
		t.Fatalf("past departure: %+v", a)
	}
}

func TestDescribeDeadline(t *testing.T) {
	cfg := testConfig()
	if got := describeDeadline(cfg.PlowingSchedule.WeekendsDeadline, "arrival"); got != "by 11:00, 2 days before" {
		t.Fatal(got)
	}
	if got := describeDeadline(cfg.PlowingSchedule.WorkDaysDeadline, "departure"); got != "by 23:00 the same day" {
		t.Fatal(got)
	}
	wd := 4
	d := cfg.PlowingSchedule.WeekendsDeadline
	d.ArrivalDeadlineWeekday = &wd
	if got := describeDeadline(d, "arrival"); got != "by 11:00 on the Thursday before" {
		t.Fatal(got)
	}
}
