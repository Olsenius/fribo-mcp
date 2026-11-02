package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// applicationID identifies the Fribo customer portal. It is public and comes
// from the portal's JavaScript bundle.
const applicationID = "19B34303-D365-4D2B-B033-90D1E535B967"

// tokenMargin renews the access token a little before it expires.
const tokenMargin = 30 * time.Second

// client talks to the Fribo platform API behind portal.hyttetjenester.no.
//
// The access token lives in memory only. When it is about to expire, or the
// API answers 401, the client logs in again with e-mail and password. The
// portal's refresh token is not used: it rotates on every use and lasts a day,
// so a fresh login is simpler and never leaves a stale token behind.
type client struct {
	base     string
	email    string
	password string
	http     *http.Client
	now      func() time.Time

	mu      sync.Mutex // guards the token and serializes logins
	token   string
	userID  string
	expires time.Time
}

func newClient(base, email, password string) *client {
	return &client{
		base:     strings.TrimRight(base, "/"),
		email:    email,
		password: password,
		http:     &http.Client{Timeout: 30 * time.Second},
		now:      time.Now,
	}
}

// APIError is an error answer from Fribo, with its machine-readable code.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if hint, ok := errorHints[e.Code]; ok {
		return fmt.Sprintf("Fribo: %s (%s)", hint, e.Code)
	}
	if e.Code != "" {
		return fmt.Sprintf("Fribo: %s (%s, HTTP %d)", e.Message, e.Code, e.Status)
	}
	return fmt.Sprintf("Fribo answered HTTP %d", e.Status)
}

// errorHints explains the codes a stay can be rejected with.
var errorHints = map[string]string{
	"invalid_credentials":               "wrong e-mail or password",
	"user_not_found":                    "no Fribo user with this e-mail",
	"stay_overlap":                      "the stay overlaps another registered stay",
	"stay_length_limit":                 "the stay is longer than the contract allows",
	"future_stays_limit":                "too many future stays are registered already",
	"no_clips_available":                "no plowing clips left on the contract",
	"arrival_time_in_past":              "the arrival time is in the past",
	"stay_arrival_too_early":            "the arrival is earlier than allowed",
	"stay_out_of_plowing_season":        "the dates are outside the plowing season",
	"first_hour_plowing_validation":     "the arrival hour is before the first plowing hour",
	"reject_arrival_day_not_allowed":    "plowing cannot be skipped on the arrival day",
	"reject_outside_stay":               "a day to skip is outside the stay",
	"reject_deadline_passed":            "it is too late to skip plowing on that day",
	"overdue_payment_request":           "there is an overdue payment request",
	"exceed_minimal_clip_level":         "too few plowing clips left",
	"contract_not_found":                "no plowing contract found for the cabin",
	"agreement_not_found":               "no plowing agreement found for the cabin",
	"stay_started":                      "the stay has already started",
	"stay_plowed":                       "the road has already been plowed for this stay",
	"stay_not_found":                    "no such stay",
	"stay_edit_forbidden":               "this stay cannot be changed",
	"stay_delete_forbidden":             "this stay cannot be deleted",
	"stay_departure_passed":             "the departure has passed",
	"stay_change_after_departure":       "the stay cannot be changed after departure",
	"stay_start_change_not_allowed":     "the arrival can no longer be changed",
	"stay_arrival_deadline_violation":   "the deadline for changing the arrival has passed",
	"stay_departure_deadline_violation": "the deadline for changing the departure has passed",
	"reject_revert_deadline":            "it is too late to undo a skipped plowing day",
	"reject_add_after_day_started":      "plowing cannot be skipped on a day that has started",
}

// Login state is kept per process. Concurrent callers wait for one login.
func (c *client) auth(ctx context.Context) (token, userID string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Add(tokenMargin).Before(c.expires) {
		return c.token, c.userID, nil
	}
	if err := c.login(ctx); err != nil {
		return "", "", err
	}
	return c.token, c.userID, nil
}

// invalidate forgets token if it is still the current one.
func (c *client) invalidate(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token {
		c.token = ""
	}
}

// login runs with c.mu held.
func (c *client) login(ctx context.Context) error {
	body := map[string]string{"email": c.email, "password": c.password, "applicationId": applicationID}
	var out struct {
		AccessToken           string    `json:"accessToken"`
		AccessTokenExpiration time.Time `json:"accessTokenExpiration"`
	}
	status, err := c.send(ctx, http.MethodPost, "/tokens/emails", "/tokens/emails", "", "", body, &out)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && status < 500 {
			return fmt.Errorf("login to Fribo failed: %w", err)
		}
		return err
	}
	uid, err := tokenSubject(out.AccessToken)
	if err != nil {
		return fmt.Errorf("login to Fribo: %w", err)
	}
	c.token, c.userID, c.expires = out.AccessToken, uid, out.AccessTokenExpiration
	if c.expires.IsZero() {
		c.expires = c.now().Add(4 * time.Minute)
	}
	return nil
}

// tokenSubject reads the user id from the JWT without verifying it: the
// token came straight from Fribo over TLS and is only sent back to Fribo.
func tokenSubject(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("unexpected access token format")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("unexpected access token format")
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Sub == "" {
		return "", errors.New("access token has no user id")
	}
	return claims.Sub, nil
}

// call makes an authenticated request. route is the path template, used for
// tracing so that ids never end up in spans. It logs in again once on 401.
func (c *client) call(ctx context.Context, method, route, path, version string, body, out any) error {
	for attempt := 0; ; attempt++ {
		token, _, err := c.auth(ctx)
		if err != nil {
			return err
		}
		status, err := c.send(ctx, method, route, path, version, token, body, out)
		if status == http.StatusUnauthorized && attempt == 0 {
			c.invalidate(token)
			continue
		}
		return err
	}
}

func (c *client) send(ctx context.Context, method, route, path, version, token string, body, out any) (int, error) {
	ctx, span := otel.Tracer("fribo-mcp").Start(ctx, "fribo "+method+" "+route, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("http.request.method", method), attribute.String("url.template", route), attribute.String("server.address", hostOf(c.base))))
	defer span.End()

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if version != "" {
		req.Header.Set("api-version", version)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", err)))
		span.SetStatus(codes.Error, "Fribo request failed")
		return 0, fmt.Errorf("Fribo request failed: %w", err)
	}
	defer resp.Body.Close()
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		span.SetStatus(codes.Error, "Fribo response failed")
		return resp.StatusCode, fmt.Errorf("read Fribo response: %w", err)
	}
	if resp.StatusCode >= 400 {
		span.SetStatus(codes.Error, http.StatusText(resp.StatusCode))
		apiErr := &APIError{Status: resp.StatusCode}
		var e struct{ Code, Message string }
		if json.Unmarshal(data, &e) == nil {
			apiErr.Code, apiErr.Message = e.Code, e.Message
		}
		return resp.StatusCode, apiErr
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("unexpected answer from Fribo for %s %s: %w", method, route, err)
		}
	}
	return resp.StatusCode, nil
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// ---------- API types ----------

// Cabin is a cabin the user has access to.
type Cabin struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default"`
}

// Contract is a plowing agreement for a cabin.
type Contract struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Type           string     `json:"type"`
	Status         string     `json:"status"`
	OrganizationID string     `json:"-"`
	StartDate      time.Time  `json:"start_date"`
	EndDate        time.Time  `json:"end_date"`
	LastPlowed     *time.Time `json:"last_plowed,omitempty"`
	UsedClips      *float64   `json:"used_clips,omitempty"`
	BoughtClips    *float64   `json:"bought_clips,omitempty"`
}

// Deadline is one set of order deadlines (work days or weekends).
type Deadline struct {
	ArrivalDeadline        int       `json:"arrivalDeadline"`
	ArrivalDeadlineWeekday *int      `json:"arrivalDeadlineWeekday"`
	ArrivalDeadlineHours   time.Time `json:"arrivalDeadlineHours"`
	DepartureDeadline      int       `json:"departureDeadline"`
	DepartureDeadlineHours time.Time `json:"departureDeadlineHours"`
}

// DateRange is a span of days, used for exception days.
type DateRange struct {
	StartDate time.Time `json:"startDate"`
	EndDate   time.Time `json:"endDate"`
}

// PlowingConfig holds the plowing company's season and deadlines.
type PlowingConfig struct {
	StartSeasonDate      time.Time  `json:"startSeasonDate"`
	EndSeasonDate        time.Time  `json:"endSeasonDate"`
	AllowDeadlineOrder   bool       `json:"allowDeadlineOrder"`
	FirstHourPlowing     *time.Time `json:"firstHourPlowing"`
	PossibleArrivalHours []int      `json:"possibleArrivalHours"`
	PlowingSchedule      struct {
		WorkDays             []string    `json:"workDays"`
		WorkDaysDeadline     Deadline    `json:"workDaysDeadline"`
		WeekendsDeadline     Deadline    `json:"weekendsDeadline"`
		WorkingExceptionDays []DateRange `json:"workingExceptionDays"`
	} `json:"plowingSchedule"`
}

// Stay is a registered period at the cabin (a "customer presence").
type Stay struct {
	ID                string    `json:"id"`
	Arrival           time.Time `json:"arrival"`
	Departure         time.Time `json:"departure"`
	SkipPlowingDates  []string  `json:"skip_plowing_dates,omitempty"`
	PlowingGuaranteed bool      `json:"plowing_guaranteed"`
	ClipWasConsumed   bool      `json:"clip_was_consumed"`
	VisitorID         string    `json:"visitor_id"`
	Visitor           string    `json:"visitor"`
	CreatedAt         time.Time `json:"created_at"`
}

// PlowEvent is one entry in the cabin's plowing history.
type PlowEvent struct {
	Time         time.Time `json:"time"`
	Type         string    `json:"type"`
	ClipConsumed float64   `json:"clip_consumed,omitempty"`
}

// ---------- API calls ----------

func (c *client) UserID(ctx context.Context) (string, error) {
	_, uid, err := c.auth(ctx)
	return uid, err
}

func (c *client) Cabins(ctx context.Context) ([]Cabin, error) {
	uid, err := c.UserID(ctx)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		IsDefault bool   `json:"isDefault"`
	}
	if err := c.call(ctx, http.MethodGet, "/cabins/byOwner/{userId}", "/cabins/byOwner/"+url.PathEscape(uid), "", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Cabin, 0, len(raw))
	for _, r := range raw {
		out = append(out, Cabin{ID: r.ID, Name: r.Name, IsDefault: r.IsDefault})
	}
	return out, nil
}

func (c *client) Contracts(ctx context.Context, cabinID string) ([]Contract, error) {
	var raw []struct {
		ID             string     `json:"id"`
		Name           string     `json:"name"`
		Type           string     `json:"type"`
		Status         string     `json:"status"`
		OrganizationID string     `json:"organizationId"`
		StartDate      time.Time  `json:"startDate"`
		EndDate        time.Time  `json:"endDate"`
		LastPlowed     *time.Time `json:"lastPlowed"`
		UsedClips      *float64   `json:"usedClips"`
		BoughtClips    *float64   `json:"boughtClips"`
	}
	path := "/plowing/" + url.PathEscape(cabinID) + "/cabinContractPreview?validByDate=true"
	if err := c.call(ctx, http.MethodGet, "/plowing/{cabinId}/cabinContractPreview", path, "2.0", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Contract, 0, len(raw))
	for _, r := range raw {
		out = append(out, Contract{ID: r.ID, Name: r.Name, Type: r.Type, Status: r.Status, OrganizationID: r.OrganizationID,
			StartDate: r.StartDate, EndDate: r.EndDate, LastPlowed: r.LastPlowed, UsedClips: r.UsedClips, BoughtClips: r.BoughtClips})
	}
	return out, nil
}

func (c *client) PlowingConfig(ctx context.Context, organizationID string) (*PlowingConfig, error) {
	var cfg PlowingConfig
	path := "/plowing/" + url.PathEscape(organizationID) + "/configurations"
	if err := c.call(ctx, http.MethodGet, "/plowing/{organizationId}/configurations", path, "", nil, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

type rawStay struct {
	ID                string    `json:"id"`
	Arrival           time.Time `json:"arrival"`
	Departure         time.Time `json:"departure"`
	RejectedDates     []string  `json:"rejectedDates"`
	PlowingGuaranteed bool      `json:"plowingGuaranteed"`
	ClipWasConsumed   bool      `json:"clipWasConsumed"`
	CreatedAt         time.Time `json:"createdAt"`
	VisitingCustomer  struct {
		ID        string `json:"id"`
		FirstName string `json:"firstName"`
	} `json:"visitingCustomer"`
}

func (r rawStay) stay() Stay {
	return Stay{ID: r.ID, Arrival: r.Arrival, Departure: r.Departure, SkipPlowingDates: r.RejectedDates,
		PlowingGuaranteed: r.PlowingGuaranteed, ClipWasConsumed: r.ClipWasConsumed,
		VisitorID: r.VisitingCustomer.ID, Visitor: r.VisitingCustomer.FirstName, CreatedAt: r.CreatedAt}
}

// UpcomingStays returns stays that have not ended, as the portal's dashboard
// shows them. Only the preview endpoint lists new stays; it holds no
// guarantee or skipped days, so each stay is read in full.
func (c *client) UpcomingStays(ctx context.Context, cabinID, agreementID string) ([]Stay, error) {
	var res struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	path := fmt.Sprintf("/plowing/customerPresences/%s/preview?page=1&size=500&staysMode=OnlyFuture&cabinContractId=%s",
		url.PathEscape(cabinID), url.QueryEscape(agreementID))
	if err := c.call(ctx, http.MethodGet, "/plowing/customerPresences/{cabinId}/preview", path, "2.0", nil, &res); err != nil {
		return nil, err
	}
	out := make([]Stay, 0, len(res.Data))
	for _, r := range res.Data {
		s, err := c.Stay(ctx, cabinID, r.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, nil
}

// PastStays returns earlier stays, newest first, reading up to max entries.
// This list did not include stays registered for the 2026/27 winter.
func (c *client) PastStays(ctx context.Context, cabinID string, max int) ([]Stay, error) {
	const size = 50
	var out []Stay
	for page := 1; len(out) < max; page++ {
		var res struct {
			Total int       `json:"total"`
			Data  []rawStay `json:"data"`
		}
		path := fmt.Sprintf("/plowing/customerPresences/%s?page=%d&size=%d", url.PathEscape(cabinID), page, size)
		if err := c.call(ctx, http.MethodGet, "/plowing/customerPresences/{cabinId}", path, "2.0", nil, &res); err != nil {
			return nil, err
		}
		for _, r := range res.Data {
			out = append(out, r.stay())
		}
		if len(res.Data) == 0 || len(out) >= res.Total {
			break
		}
	}
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

func (c *client) Stay(ctx context.Context, cabinID, stayID string) (*Stay, error) {
	var r rawStay
	path := "/plowing/customerPresences/" + url.PathEscape(cabinID) + "/" + url.PathEscape(stayID)
	if err := c.call(ctx, http.MethodGet, "/plowing/customerPresences/{cabinId}/{stayId}", path, "2.0", nil, &r); err != nil {
		return nil, err
	}
	s := r.stay()
	return &s, nil
}

func (c *client) PlowingHistory(ctx context.Context, cabinID string, max int) ([]PlowEvent, error) {
	const size = 50
	var out []PlowEvent
	for page := 1; len(out) < max; page++ {
		var res struct {
			Total int `json:"total"`
			Data  []struct {
				EventDate        time.Time `json:"eventDate"`
				PlowingEventType string    `json:"plowingEventType"`
				ClipConsumed     float64   `json:"clipConsumed"`
			} `json:"data"`
		}
		path := fmt.Sprintf("/plowing/cabin/%s/history?page=%d&size=%d", url.PathEscape(cabinID), page, size)
		if err := c.call(ctx, http.MethodGet, "/plowing/cabin/{cabinId}/history", path, "2.0", nil, &res); err != nil {
			return nil, err
		}
		for _, e := range res.Data {
			out = append(out, PlowEvent{Time: e.EventDate, Type: e.PlowingEventType, ClipConsumed: e.ClipConsumed})
		}
		if len(res.Data) == 0 || len(out) >= res.Total {
			break
		}
	}
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

// stayBody is what the portal sends when it creates or changes a stay.
type stayBody struct {
	CabinID          string   `json:"cabinId,omitempty"`
	VisitingCustomer string   `json:"visitingCustomerId,omitempty"`
	Arrival          string   `json:"arrival"`
	Departure        string   `json:"departure"`
	RejectedDays     []string `json:"rejectedDays"`
	Comment          *string  `json:"comment"`
}

// isoMillis matches JavaScript's Date.toISOString.
func isoMillis(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func (c *client) CreateStay(ctx context.Context, cabinID string, p stayPlan) error {
	uid, err := c.UserID(ctx)
	if err != nil {
		return err
	}
	body := stayBody{CabinID: cabinID, VisitingCustomer: uid, Arrival: isoMillis(p.Arrival), Departure: isoMillis(p.Departure), RejectedDays: p.skipDays()}
	return c.call(ctx, http.MethodPost, "/plowing/customerPresences", "/plowing/customerPresences", "2.0", body, nil)
}

func (c *client) UpdateStay(ctx context.Context, stayID string, p stayPlan) error {
	body := stayBody{Arrival: isoMillis(p.Arrival), Departure: isoMillis(p.Departure), RejectedDays: p.skipDays()}
	return c.call(ctx, http.MethodPut, "/plowing/customerPresences/{stayId}", "/plowing/customerPresences/"+url.PathEscape(stayID), "2.0", body, nil)
}

func (c *client) DeleteStay(ctx context.Context, stayID string) error {
	return c.call(ctx, http.MethodDelete, "/plowing/customerPresences/{stayId}", "/plowing/customerPresences/"+url.PathEscape(stayID), "2.0", nil, nil)
}
