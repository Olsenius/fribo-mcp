package main

import (
	"fmt"
	"slices"
	"time"
)

// Fribo works in Norwegian local time: stays start at a whole hour and end
// at 23:59:59 on the departure day, and deadlines are local clock times.
var oslo = mustLoad("Europe/Oslo")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

const dateLayout = "2006-01-02"

// day is midnight in Oslo on t's local date.
func day(t time.Time) time.Time {
	y, m, d := t.In(oslo).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, oslo)
}

func addDays(t time.Time, n int) time.Time {
	y, m, d := t.In(oslo).Date()
	return time.Date(y, m, d+n, 0, 0, 0, 0, oslo)
}

func parseDate(field, v string) (time.Time, error) {
	t, err := time.ParseInLocation(dateLayout, v, oslo)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be a date like 2026-12-18", field)
	}
	return t, nil
}

// stayPlan is a stay as it will be sent to Fribo.
type stayPlan struct {
	Arrival   time.Time
	Departure time.Time
	Skip      []time.Time
}

func (p stayPlan) skipDays() []string {
	out := []string{}
	for _, d := range p.Skip {
		out = append(out, d.Format(dateLayout))
	}
	return out
}

// buildPlan turns dates and an arrival hour into the times the portal uses.
func buildPlan(arrivalDate string, arrivalHour int, departureDate string, skip []string) (stayPlan, error) {
	arr, err := parseDate("arrival_date", arrivalDate)
	if err != nil {
		return stayPlan{}, err
	}
	dep, err := parseDate("departure_date", departureDate)
	if err != nil {
		return stayPlan{}, err
	}
	if arrivalHour < 0 || arrivalHour > 23 {
		return stayPlan{}, fmt.Errorf("arrival_hour must be 0-23")
	}
	if dep.Before(arr) {
		return stayPlan{}, fmt.Errorf("departure_date is before arrival_date")
	}
	p := stayPlan{
		Arrival:   time.Date(arr.Year(), arr.Month(), arr.Day(), arrivalHour, 0, 0, 0, oslo),
		Departure: time.Date(dep.Year(), dep.Month(), dep.Day(), 23, 59, 59, 0, oslo),
	}
	for _, s := range skip {
		d, err := parseDate("skip_plowing_dates", s)
		if err != nil {
			return stayPlan{}, err
		}
		if d.Before(arr) || d.After(dep) {
			return stayPlan{}, fmt.Errorf("skip_plowing_dates: %s is outside the stay", s)
		}
		if !slices.ContainsFunc(p.Skip, d.Equal) {
			p.Skip = append(p.Skip, d)
		}
	}
	return p, nil
}

// weekendLike follows the portal: Friday to Sunday and the company's
// exception days use the weekend deadlines.
func weekendLike(target time.Time, cfg *PlowingConfig) bool {
	switch target.In(oslo).Weekday() {
	case time.Friday, time.Saturday, time.Sunday:
		return true
	}
	t := day(target)
	for _, r := range cfg.PlowingSchedule.WorkingExceptionDays {
		if !t.Before(day(r.StartDate)) && !t.After(day(r.EndDate)) {
			return true
		}
	}
	return false
}

func deadlineSet(target time.Time, cfg *PlowingConfig) (Deadline, bool) {
	if weekendLike(target, cfg) {
		return cfg.PlowingSchedule.WeekendsDeadline, true
	}
	return cfg.PlowingSchedule.WorkDaysDeadline, false
}

// earliestDay ports the portal's deadline rule: a day can be ordered when it
// is at least N days after today, or N+1 days once today's cut-off time has
// passed. kind is "arrival" or "departure".
func earliestDay(target time.Time, kind string, cfg *PlowingConfig, now time.Time) time.Time {
	d, weekend := deadlineSet(target, cfg)
	days, cutoff := d.DepartureDeadline, d.DepartureDeadlineHours
	if kind == "arrival" {
		days, cutoff = d.ArrivalDeadline, d.ArrivalDeadlineHours
		if weekend && d.ArrivalDeadlineWeekday != nil {
			days = (int(target.In(oslo).Weekday()) - *d.ArrivalDeadlineWeekday + 7) % 7
			if days == 0 {
				days = 7
			}
		}
	}
	c := cutoff.In(oslo)
	today := day(now)
	limit := time.Date(today.Year(), today.Month(), today.Day(), c.Hour(), c.Minute(), 0, 0, oslo)
	earliest := addDays(today, days)
	if !now.Before(limit) {
		earliest = addDays(earliest, 1)
	}
	return earliest
}

// describeDeadline says in words when a day must be ordered.
func describeDeadline(d Deadline, kind string) string {
	days, cutoff := d.DepartureDeadline, d.DepartureDeadlineHours
	if kind == "arrival" {
		days, cutoff = d.ArrivalDeadline, d.ArrivalDeadlineHours
		if d.ArrivalDeadlineWeekday != nil {
			return fmt.Sprintf("by %s on the %s before", cutoff.In(oslo).Format("15:04"), time.Weekday(*d.ArrivalDeadlineWeekday))
		}
	}
	at := cutoff.In(oslo).Format("15:04")
	switch days {
	case 0:
		return "by " + at + " the same day"
	case 1:
		return "by " + at + " the day before"
	default:
		return fmt.Sprintf("by %s, %d days before", at, days)
	}
}

// earliestGuaranteedArrival is the first day, from today, that can still be
// ordered in time. Each day may use other deadlines, as in the portal.
func earliestGuaranteedArrival(cfg *PlowingConfig, now time.Time) string {
	for i := 0; i < 366; i++ {
		c := addDays(day(now), i)
		if !c.Before(earliestDay(c, "arrival", cfg, now)) {
			return c.Format(dateLayout)
		}
	}
	return ""
}

// Assessment tells whether a stay meets the contract's deadlines and rules.
// Fribo has the final word; these checks mirror what the portal shows.
type Assessment struct {
	PlowingGuaranteed         bool     `json:"plowing_guaranteed"`
	EarliestGuaranteedArrival string   `json:"earliest_guaranteed_arrival"`
	ArrivalDeadline           string   `json:"arrival_deadline"`
	Problems                  []string `json:"problems,omitempty"`
	Notes                     []string `json:"notes,omitempty"`
}

func firstPlowingHour(cfg *PlowingConfig) (int, bool) {
	if cfg.FirstHourPlowing == nil {
		return 0, false
	}
	return cfg.FirstHourPlowing.In(oslo).Hour(), true
}

func inSeason(t time.Time, cfg *PlowingConfig) bool {
	if cfg.StartSeasonDate.IsZero() || cfg.EndSeasonDate.IsZero() {
		return true
	}
	d := day(t)
	return !d.Before(day(cfg.StartSeasonDate)) && !d.After(day(cfg.EndSeasonDate))
}

// assess checks a planned stay. arrivalChanged is false when an existing
// stay keeps its arrival, so only the departure deadline applies.
func assess(p stayPlan, arrivalChanged bool, cfg *PlowingConfig, now time.Time) Assessment {
	d, _ := deadlineSet(p.Arrival, cfg)
	a := Assessment{
		PlowingGuaranteed: true,
		ArrivalDeadline:   describeDeadline(d, "arrival"),
	}
	earliest := earliestDay(p.Arrival, "arrival", cfg, now)
	a.EarliestGuaranteedArrival = earliestGuaranteedArrival(cfg, now)
	if arrivalChanged {
		if !p.Arrival.After(now) {
			a.Problems = append(a.Problems, "the arrival time is in the past")
		}
		if day(p.Arrival).Before(earliest) {
			a.PlowingGuaranteed = false
			msg := "registered after the deadline: plowing before arrival is not guaranteed"
			if !cfg.AllowDeadlineOrder {
				msg += " (Fribo usually accepts the stay, but the driver may not make it in time)"
			}
			a.Notes = append(a.Notes, msg)
		}
		if h, ok := firstPlowingHour(cfg); ok && p.Arrival.In(oslo).Hour() < h {
			a.Problems = append(a.Problems, fmt.Sprintf("arrival_hour is before the first plowing hour (%02d:00)", h))
		}
		if len(cfg.PossibleArrivalHours) > 0 && !slices.Contains(cfg.PossibleArrivalHours, p.Arrival.In(oslo).Hour()) {
			a.Problems = append(a.Problems, fmt.Sprintf("arrival_hour must be one of %v", cfg.PossibleArrivalHours))
		}
	}
	if !p.Departure.After(now) {
		a.Problems = append(a.Problems, "the departure is in the past")
	} else if dep := earliestDay(p.Departure, "departure", cfg, now); day(p.Departure).Before(dep) && !arrivalChanged {
		set, _ := deadlineSet(p.Departure, cfg)
		a.Problems = append(a.Problems, "too late to change the departure to this day (it must be changed "+describeDeadline(set, "departure")+")")
	}
	// The season in Fribo's settings can be out of date: in November 2026 it
	// still ended in June, yet new stays were accepted and guaranteed.
	if !inSeason(p.Arrival, cfg) || !inSeason(p.Departure, cfg) {
		a.Notes = append(a.Notes, fmt.Sprintf("outside the season in Fribo's settings (%s to %s); Fribo has still accepted such stays",
			day(cfg.StartSeasonDate).Format(dateLayout), day(cfg.EndSeasonDate).Format(dateLayout)))
	}
	if len(a.Problems) > 0 {
		a.PlowingGuaranteed = false
	}
	return a
}
