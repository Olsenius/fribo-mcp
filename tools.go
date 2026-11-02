package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const instructions = `Tools for Fribo (portal.hyttetjenester.no), the cabin road plowing service.
Register a stay (arrival and departure) so the road to the cabin is plowed before arrival. Times are Norwegian local time; dates are YYYY-MM-DD.
Start with plowing_status: it shows the contract, the order deadline, the earliest arrival with guaranteed plowing and the upcoming stays.
Before register_stay or update_stay, run check_stay with the same dates and show the result. Always confirm with the user before register_stay, update_stay or cancel_stay.
cabin_id can be left empty when the user has one cabin, or a default cabin is set.`

func ptr[T any](v T) *T { return &v }

var (
	readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)}
	changes  = &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)}
	removes  = &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(true)}
)

// nowFunc is replaced in tests.
var nowFunc = time.Now

type cabinIn struct {
	CabinID string `json:"cabin_id,omitempty" jsonschema:"cabin id from list_cabins; empty means the default cabin"`
}

type cabinsOut struct {
	Cabins []Cabin `json:"cabins"`
}

type seasonOut struct {
	Start  string `json:"start"`
	End    string `json:"end"`
	Active bool   `json:"active"`
}

type deadlinesOut struct {
	WorkDaysArrival   string `json:"work_days_arrival"`
	WeekendArrival    string `json:"weekend_arrival"`
	WorkDaysDeparture string `json:"work_days_departure"`
	WeekendDeparture  string `json:"weekend_departure"`
	Note              string `json:"note"`
}

type statusOut struct {
	CabinID                   string       `json:"cabin_id"`
	Contract                  Contract     `json:"contract"`
	Season                    seasonOut    `json:"season"`
	Deadlines                 deadlinesOut `json:"deadlines"`
	FirstPlowingHour          *int         `json:"first_plowing_hour,omitempty"`
	AllowedArrivalHours       []int        `json:"allowed_arrival_hours,omitempty"`
	EarliestGuaranteedArrival string       `json:"earliest_guaranteed_arrival"`
	LastPlowed                *time.Time   `json:"last_plowed,omitempty"`
	UpcomingStays             []Stay       `json:"upcoming_stays"`
	Warnings                  []string     `json:"warnings,omitempty"`
}

type staysIn struct {
	CabinID     string `json:"cabin_id,omitempty" jsonschema:"cabin id from list_cabins; empty means the default cabin"`
	IncludePast bool   `json:"include_past,omitempty" jsonschema:"also list stays that have ended"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max stays to return (default 20)"`
}
type staysOut struct {
	Count int    `json:"count"`
	Stays []Stay `json:"stays"`
}

type historyIn struct {
	CabinID string `json:"cabin_id,omitempty" jsonschema:"cabin id from list_cabins; empty means the default cabin"`
	Limit   int    `json:"limit,omitempty" jsonschema:"max events to return (default 20)"`
}
type historyOut struct {
	Events []PlowEvent `json:"events"`
}

type stayIn struct {
	CabinID          string   `json:"cabin_id,omitempty" jsonschema:"cabin id from list_cabins; empty means the default cabin"`
	ArrivalDate      string   `json:"arrival_date" jsonschema:"arrival day, YYYY-MM-DD"`
	ArrivalHour      int      `json:"arrival_hour" jsonschema:"expected arrival hour, 0-23 Norwegian time; the road should be plowed by then"`
	DepartureDate    string   `json:"departure_date" jsonschema:"departure day, YYYY-MM-DD; the stay lasts until 23:59 that day"`
	SkipPlowingDates []string `json:"skip_plowing_dates,omitempty" jsonschema:"days within the stay that need no plowing, YYYY-MM-DD"`
}

type plannedStay struct {
	Arrival          time.Time `json:"arrival"`
	Departure        time.Time `json:"departure"`
	SkipPlowingDates []string  `json:"skip_plowing_dates,omitempty"`
}

type checkOut struct {
	CabinID    string      `json:"cabin_id"`
	Stay       plannedStay `json:"stay"`
	Assessment Assessment  `json:"assessment"`
	Overlaps   []string    `json:"overlapping_stay_ids,omitempty"`
}

type registerOut struct {
	Registered bool       `json:"registered"`
	Stay       *Stay      `json:"stay,omitempty"`
	Assessment Assessment `json:"assessment"`
}

type updateIn struct {
	StayID           string   `json:"stay_id" jsonschema:"stay id from list_stays"`
	CabinID          string   `json:"cabin_id,omitempty" jsonschema:"cabin id from list_cabins; empty means the default cabin"`
	ArrivalDate      string   `json:"arrival_date,omitempty" jsonschema:"new arrival day, YYYY-MM-DD; empty keeps it"`
	ArrivalHour      *int     `json:"arrival_hour,omitempty" jsonschema:"new arrival hour, 0-23; empty keeps it"`
	DepartureDate    string   `json:"departure_date,omitempty" jsonschema:"new departure day, YYYY-MM-DD; empty keeps it"`
	SkipPlowingDates []string `json:"skip_plowing_dates,omitempty" jsonschema:"replaces the days that need no plowing; empty keeps them"`
}

type cancelIn struct {
	StayID string `json:"stay_id" jsonschema:"stay id from list_stays"`
}
type cancelOut struct {
	Cancelled string `json:"cancelled"`
}

func limitOr(n, def int) int {
	if n <= 0 {
		return def
	}
	return n
}

// localize shows stay times in Norwegian time.
func localize(s Stay) Stay {
	s.Arrival, s.Departure, s.CreatedAt = s.Arrival.In(oslo), s.Departure.In(oslo), s.CreatedAt.In(oslo)
	return s
}

// resolveCabin picks the cabin: the argument, FRIBO_CABIN_ID, the
// default cabin, or the only one.
func resolveCabin(ctx context.Context, c *client, s Settings, id string) (string, error) {
	if id = strings.TrimSpace(id); id != "" {
		return id, nil
	}
	if s.CabinID != "" {
		return s.CabinID, nil
	}
	cabins, err := c.Cabins(ctx)
	if err != nil {
		return "", err
	}
	if len(cabins) == 1 {
		return cabins[0].ID, nil
	}
	for _, cb := range cabins {
		if cb.IsDefault {
			return cb.ID, nil
		}
	}
	if len(cabins) == 0 {
		return "", errors.New("no cabins on this Fribo account")
	}
	return "", errors.New("several cabins and none is the default: pass cabin_id from list_cabins")
}

// plowingContract picks the agreement stays are registered against, the
// same way the portal does: a permanent stay agreement, else any active
// agreement that is not a common road.
func plowingContract(contracts []Contract) (*Contract, error) {
	var pick *Contract
	for i, ct := range contracts {
		if ct.Type == "CommonRoad" {
			continue
		}
		if ct.Type == "PermanentStay" {
			return &contracts[i], nil
		}
		if pick == nil {
			pick = &contracts[i]
		}
	}
	if pick == nil {
		return nil, errors.New("the cabin has no plowing contract in Fribo")
	}
	return pick, nil
}

type cabinContext struct {
	client   *client
	cabinID  string
	contract *Contract
	config   *PlowingConfig
}

func (a *app) cabinContext(ctx context.Context, cabinID string) (*cabinContext, error) {
	c, s, err := a.client()
	if err != nil {
		return nil, err
	}
	id, err := resolveCabin(ctx, c, s, cabinID)
	if err != nil {
		return nil, err
	}
	contracts, err := c.Contracts(ctx, id)
	if err != nil {
		return nil, err
	}
	ct, err := plowingContract(contracts)
	if err != nil {
		return nil, err
	}
	cfg, err := c.PlowingConfig(ctx, ct.OrganizationID)
	if err != nil {
		return nil, err
	}
	return &cabinContext{client: c, cabinID: id, contract: ct, config: cfg}, nil
}

func upcoming(stays []Stay, now time.Time) []Stay {
	out := []Stay{}
	for _, s := range stays {
		if s.Departure.After(now) {
			out = append(out, localize(s))
		}
	}
	slices.SortFunc(out, func(a, b Stay) int { return a.Arrival.Compare(b.Arrival) })
	return out
}

func overlaps(stays []Stay, p stayPlan, except string) []string {
	var ids []string
	for _, s := range stays {
		if s.ID != except && s.Arrival.Before(p.Departure) && p.Arrival.Before(s.Departure) {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

func planned(p stayPlan) plannedStay {
	return plannedStay{Arrival: p.Arrival, Departure: p.Departure, SkipPlowingDates: p.skipDays()}
}

// findStay looks up a stay by its arrival after it has been created.
func findStay(stays []Stay, arrival time.Time) *Stay {
	for _, s := range stays {
		if s.Arrival.Equal(arrival) {
			s = localize(s)
			return &s
		}
	}
	return nil
}

func newServer(a *app) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fribo", Title: "Fribo brøyting", Version: version}, &mcp.ServerOptions{Instructions: instructions})
	s.AddReceivingMiddleware(traceMCP)

	mcp.AddTool(s, &mcp.Tool{Name: "list_cabins", Description: "List the cabins on the Fribo account.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, cabinsOut, error) {
			c, _, err := a.client()
			if err != nil {
				return nil, cabinsOut{}, err
			}
			cabins, err := c.Cabins(ctx)
			return nil, cabinsOut{Cabins: cabins}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "plowing_status", Description: "Plowing contract, season, order deadlines, earliest arrival with guaranteed plowing, when the road was last plowed and the upcoming stays.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in cabinIn) (*mcp.CallToolResult, statusOut, error) {
			cc, err := a.cabinContext(ctx, in.CabinID)
			if err != nil {
				return nil, statusOut{}, err
			}
			now := nowFunc()
			stays, err := cc.client.UpcomingStays(ctx, cc.cabinID, cc.contract.ID)
			if err != nil {
				return nil, statusOut{}, err
			}
			cfg := cc.config
			out := statusOut{
				CabinID:  cc.cabinID,
				Contract: *cc.contract,
				Season: seasonOut{Start: day(cfg.StartSeasonDate).Format(dateLayout), End: day(cfg.EndSeasonDate).Format(dateLayout),
					Active: inSeason(now, cfg)},
				Deadlines: deadlinesOut{
					WorkDaysArrival:   describeDeadline(cfg.PlowingSchedule.WorkDaysDeadline, "arrival"),
					WeekendArrival:    describeDeadline(cfg.PlowingSchedule.WeekendsDeadline, "arrival"),
					WorkDaysDeparture: describeDeadline(cfg.PlowingSchedule.WorkDaysDeadline, "departure"),
					WeekendDeparture:  describeDeadline(cfg.PlowingSchedule.WeekendsDeadline, "departure"),
					Note:              "Weekend deadlines apply to Friday, Saturday, Sunday and the company's exception days. Departure deadlines apply when changing a registered stay.",
				},
				AllowedArrivalHours:       cfg.PossibleArrivalHours,
				EarliestGuaranteedArrival: earliestGuaranteedArrival(cfg, now),
				UpcomingStays:             upcoming(stays, now),
			}
			if h, ok := firstPlowingHour(cfg); ok {
				out.FirstPlowingHour = &h
			}
			if cc.contract.LastPlowed != nil {
				t := cc.contract.LastPlowed.In(oslo)
				out.LastPlowed = &t
			} else if ev, err := cc.client.PlowingHistory(ctx, cc.cabinID, 10); err == nil {
				for _, e := range ev {
					if e.Type == "Plow" || e.Type == "MaintenancePlow" {
						t := e.Time.In(oslo)
						out.LastPlowed = &t
						break
					}
				}
			}
			if !out.Season.Active {
				out.Warnings = append(out.Warnings, fmt.Sprintf("The season in Fribo's settings is %s to %s, so today is outside it. Fribo has still accepted and guaranteed new stays.", out.Season.Start, out.Season.End))
			}
			if cc.contract.Status != "Active" {
				out.Warnings = append(out.Warnings, "the plowing contract is "+cc.contract.Status)
			}
			return nil, out, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_stays", Description: "List upcoming stays for a cabin, soonest first, and earlier stays with include_past. Each stay shows whether plowing is guaranteed.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in staysIn) (*mcp.CallToolResult, staysOut, error) {
			cc, err := a.cabinContext(ctx, in.CabinID)
			if err != nil {
				return nil, staysOut{}, err
			}
			stays, err := cc.client.UpcomingStays(ctx, cc.cabinID, cc.contract.ID)
			if err != nil {
				return nil, staysOut{}, err
			}
			list := upcoming(stays, nowFunc())
			if in.IncludePast {
				past, err := cc.client.PastStays(ctx, cc.cabinID, 200)
				if err != nil {
					return nil, staysOut{}, err
				}
				for _, s := range past {
					if !slices.ContainsFunc(list, func(u Stay) bool { return u.ID == s.ID }) {
						list = append(list, localize(s))
					}
				}
			}
			if n := limitOr(in.Limit, 20); len(list) > n {
				list = list[:n]
			}
			if list == nil {
				list = []Stay{}
			}
			return nil, staysOut{Count: len(list), Stays: list}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "plowing_history", Description: "When the road to the cabin was plowed, newest first.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in historyIn) (*mcp.CallToolResult, historyOut, error) {
			c, st, err := a.client()
			if err != nil {
				return nil, historyOut{}, err
			}
			id, err := resolveCabin(ctx, c, st, in.CabinID)
			if err != nil {
				return nil, historyOut{}, err
			}
			ev, err := c.PlowingHistory(ctx, id, limitOr(in.Limit, 20))
			for i := range ev {
				ev[i].Time = ev[i].Time.In(oslo)
			}
			if ev == nil {
				ev = []PlowEvent{}
			}
			return nil, historyOut{Events: ev}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "check_stay", Description: "Check a planned stay against the contract's deadlines, season and other stays without registering anything.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in stayIn) (*mcp.CallToolResult, checkOut, error) {
			p, err := buildPlan(in.ArrivalDate, in.ArrivalHour, in.DepartureDate, in.SkipPlowingDates)
			if err != nil {
				return nil, checkOut{}, err
			}
			cc, err := a.cabinContext(ctx, in.CabinID)
			if err != nil {
				return nil, checkOut{}, err
			}
			stays, err := cc.client.UpcomingStays(ctx, cc.cabinID, cc.contract.ID)
			if err != nil {
				return nil, checkOut{}, err
			}
			out := checkOut{CabinID: cc.cabinID, Stay: planned(p), Assessment: assess(p, true, cc.config, nowFunc()), Overlaps: overlaps(stays, p, "")}
			if len(out.Overlaps) > 0 {
				out.Assessment.Problems = append(out.Assessment.Problems, "overlaps a registered stay; change that stay instead")
				out.Assessment.PlowingGuaranteed = false
			}
			return nil, out, nil
		})

	a.writeTools = []string{"register_stay", "update_stay", "cancel_stay"}
	a.addWrite = func() {
		mcp.AddTool(s, &mcp.Tool{Name: "register_stay", Description: "Register a stay at the cabin so the road is plowed before arrival. Run check_stay first and confirm with the user.", Annotations: changes},
			func(ctx context.Context, _ *mcp.CallToolRequest, in stayIn) (*mcp.CallToolResult, registerOut, error) {
				p, err := buildPlan(in.ArrivalDate, in.ArrivalHour, in.DepartureDate, in.SkipPlowingDates)
				if err != nil {
					return nil, registerOut{}, err
				}
				cc, err := a.cabinContext(ctx, in.CabinID)
				if err != nil {
					return nil, registerOut{}, err
				}
				out := registerOut{Assessment: assess(p, true, cc.config, nowFunc())}
				if err := cc.client.CreateStay(ctx, cc.cabinID, p); err != nil {
					return nil, out, err
				}
				out.Registered = true
				if stays, err := cc.client.UpcomingStays(ctx, cc.cabinID, cc.contract.ID); err == nil {
					out.Stay = findStay(stays, p.Arrival)
				}
				if out.Stay == nil {
					out.Assessment.Notes = append(out.Assessment.Notes, "Fribo accepted the stay, but it could not be read back yet; check list_stays")
				}
				return nil, out, nil
			})

		mcp.AddTool(s, &mcp.Tool{Name: "update_stay", Description: "Change the dates, arrival hour or skipped plowing days of a registered stay. Confirm with the user first.", Annotations: changes},
			func(ctx context.Context, _ *mcp.CallToolRequest, in updateIn) (*mcp.CallToolResult, registerOut, error) {
				if strings.TrimSpace(in.StayID) == "" {
					return nil, registerOut{}, errors.New("stay_id is required")
				}
				cc, err := a.cabinContext(ctx, in.CabinID)
				if err != nil {
					return nil, registerOut{}, err
				}
				cur, err := cc.client.Stay(ctx, cc.cabinID, in.StayID)
				if err != nil {
					return nil, registerOut{}, err
				}
				arr, dep := cur.Arrival.In(oslo), cur.Departure.In(oslo)
				arrivalDate, departureDate, hour := arr.Format(dateLayout), dep.Format(dateLayout), arr.Hour()
				if in.ArrivalDate != "" {
					arrivalDate = in.ArrivalDate
				}
				if in.ArrivalHour != nil {
					hour = *in.ArrivalHour
				}
				if in.DepartureDate != "" {
					departureDate = in.DepartureDate
				}
				skip := cur.SkipPlowingDates
				if in.SkipPlowingDates != nil {
					skip = in.SkipPlowingDates
				}
				p, err := buildPlan(arrivalDate, hour, departureDate, skip)
				if err != nil {
					return nil, registerOut{}, err
				}
				out := registerOut{Assessment: assess(p, !p.Arrival.Equal(cur.Arrival), cc.config, nowFunc())}
				if err := cc.client.UpdateStay(ctx, in.StayID, p); err != nil {
					return nil, out, err
				}
				out.Registered = true
				if st, err := cc.client.Stay(ctx, cc.cabinID, in.StayID); err == nil {
					l := localize(*st)
					out.Stay = &l
				}
				return nil, out, nil
			})

		mcp.AddTool(s, &mcp.Tool{Name: "cancel_stay", Description: "Delete a registered stay that has not started. Confirm with the user first.", Annotations: removes},
			func(ctx context.Context, _ *mcp.CallToolRequest, in cancelIn) (*mcp.CallToolResult, cancelOut, error) {
				if strings.TrimSpace(in.StayID) == "" {
					return nil, cancelOut{}, errors.New("stay_id is required")
				}
				c, _, err := a.client()
				if err != nil {
					return nil, cancelOut{}, err
				}
				if err := c.DeleteStay(ctx, in.StayID); err != nil {
					return nil, cancelOut{}, err
				}
				return nil, cancelOut{Cancelled: in.StayID}, nil
			})
	}
	return s
}
