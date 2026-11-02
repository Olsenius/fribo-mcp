package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Synthetic account and ids. Values starting with "private-" must never
// appear in tool output, logs or spans.
const (
	testEmail    = "cabin-owner@example.test"
	testPassword = "private-password"
	testUserID   = "11111111-1111-4111-8111-111111111111"
	testCabinID  = "22222222-2222-4222-8222-222222222222"
	testOrgID    = "33333333-3333-4333-8333-333333333333"
	testStayID   = "44444444-4444-4444-8444-444444444444"
	// testAgreementID is the cabin's plowing agreement.
	testAgreementID = "66666666-6666-4666-8666-666666666666"
)

type fakeFribo struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	logins     int
	issued     []string
	force401   bool
	created    []map[string]any
	updated    map[string]map[string]any
	deleted    []string
	stays      []map[string]any
	seasonEnd  string
	versionErr string
}

func fakeJWT(sub string, n int) string {
	enc := base64.RawURLEncoding.EncodeToString
	payload, _ := json.Marshal(map[string]any{"sub": sub, "email": testEmail, "phoneNumber": "private-phone", "n": n})
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".private-signature"
}

func newFakeFribo(t *testing.T) *fakeFribo {
	f := &fakeFribo{t: t, updated: map[string]map[string]any{}, seasonEnd: "2027-06-30T00:00:00.000Z"}
	// Like the real API, upcoming stays ("_future") are only in the preview
	// endpoint, and the plain list holds earlier stays.
	f.stays = []map[string]any{{
		"id": testStayID, "cabinId": testCabinID, "_future": true,
		"arrival": "2026-12-04T12:00:00.000Z", "departure": "2026-12-06T22:59:59.000Z",
		"plowingGuaranteed": true, "clipWasConsumed": false, "rejectedDates": []string{},
		"createdAt": "2026-11-01T18:00:00.000Z", "lastUpdated": "2026-11-01T18:00:00.000Z",
		"visitingCustomer": map[string]any{"id": testUserID, "firstName": "Kari", "lastName": "private-lastname"},
	}, {
		"id": "55555555-5555-4555-8555-555555555555", "cabinId": testCabinID,
		"arrival": "2026-02-20T12:00:00.000Z", "departure": "2026-02-24T22:59:59.000Z",
		"plowingGuaranteed": false, "clipWasConsumed": false, "rejectedDates": []string{},
		"createdAt": "2026-02-19T21:00:00.000Z", "lastUpdated": "2026-02-19T21:00:00.000Z",
		"visitingCustomer": map[string]any{"id": testUserID, "firstName": "Kari", "lastName": "private-lastname"},
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFribo) client() *client { return newClient(f.srv.URL, testEmail, testPassword) }

func writeAny(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (f *fakeFribo) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Traceparent") != "" || r.Header.Get("Baggage") != "" {
		f.t.Error("trace context sent to Fribo")
	}
	path := r.URL.Path
	if path == "/tokens/emails" && r.Method == http.MethodPost {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["applicationId"] != applicationID {
			f.t.Errorf("login without application id: %v", body["applicationId"])
		}
		if body["email"] != testEmail || body["password"] != testPassword {
			writeAny(w, 400, map[string]any{"code": "invalid_credentials", "message": "Invalid credentials", "data": map[string]any{}})
			return
		}
		f.logins++
		tok := fakeJWT(testUserID, f.logins)
		f.issued = append(f.issued, tok)
		http.SetCookie(w, &http.Cookie{Name: "refresh_token", Value: "private-refresh", Path: "/"})
		writeAny(w, 200, map[string]any{"accessToken": tok,
			"accessTokenExpiration":  time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339Nano),
			"refreshTokenExpiration": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339Nano)})
		return
	}
	if r.Header.Get("Cookie") != "" {
		f.t.Error("cookie sent to Fribo; the client should not keep cookies")
	}
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if f.force401 || len(f.issued) == 0 || auth != f.issued[len(f.issued)-1] {
		f.force401 = false
		writeAny(w, 401, map[string]any{"code": "unauthorized", "message": "Unauthorized"})
		return
	}
	needVersion := func(v string) bool {
		if got := r.Header.Get("api-version"); got != v {
			f.t.Errorf("%s %s: api-version %q, want %q", r.Method, path, got, v)
		}
		return true
	}
	switch {
	case r.Method == "GET" && path == "/cabins/byOwner/"+testUserID:
		writeAny(w, 200, []map[string]any{{"id": testCabinID, "name": "Fjellhytta", "address": "private-address", "isDefault": true}})
	case r.Method == "GET" && path == "/plowing/"+testCabinID+"/cabinContractPreview":
		needVersion("2.0")
		writeAny(w, 200, []map[string]any{
			{"id": testAgreementID, "contractId": "c1", "status": "Active", "name": "Brøyting", "type": "PlowingContract",
				"organizationId": testOrgID, "startDate": "2025-07-01T00:00:00.000Z", "endDate": "2028-06-30T00:00:00.000Z"},
			{"id": "77777777-7777-4777-8777-777777777777", "status": "Active", "name": "Fellesvei", "type": "CommonRoad",
				"organizationId": "other", "startDate": "2025-07-01T00:00:00.000Z", "endDate": "2028-06-30T00:00:00.000Z"},
		})
	case r.Method == "GET" && path == "/plowing/"+testOrgID+"/configurations":
		writeAny(w, 200, map[string]any{
			"startSeasonDate": "2026-07-01T00:00:00.000Z", "endSeasonDate": f.seasonEnd,
			"allowDeadlineOrder": false, "firstHourPlowing": "2026-11-02T04:00:00.000Z", "possibleArrivalHours": []int{},
			"plowingSchedule": map[string]any{
				"workDays":             []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"},
				"workDaysDeadline":     map[string]any{"arrivalDeadline": 1, "arrivalDeadlineHours": "2026-11-02T10:00:00.000Z", "departureDeadline": 0, "departureDeadlineHours": "2026-11-02T22:00:00.000Z"},
				"weekendsDeadline":     map[string]any{"arrivalDeadline": 2, "arrivalDeadlineHours": "2026-11-02T10:00:00.000Z", "departureDeadline": 0, "departureDeadlineHours": "2026-11-02T22:00:00.000Z"},
				"workingExceptionDays": []any{}, "notWorkingExceptionDays": []any{}, "nationalHolidays": []any{},
			},
		})
	case r.Method == "GET" && path == "/plowing/customerPresences/"+testCabinID:
		needVersion("2.0")
		var past []map[string]any
		for _, s := range f.stays {
			if s["_future"] != true {
				past = append(past, s)
			}
		}
		writeAny(w, 200, map[string]any{"total": len(past), "pageTotal": len(past), "data": past})
	case r.Method == "GET" && path == "/plowing/customerPresences/"+testCabinID+"/preview":
		needVersion("2.0")
		q := r.URL.Query()
		if q.Get("staysMode") != "OnlyFuture" || q.Get("cabinContractId") != testAgreementID {
			writeAny(w, 400, map[string]any{"code": "bad_request", "message": "Bad request"})
			return
		}
		var future []map[string]any
		for _, s := range f.stays {
			if s["_future"] == true {
				future = append(future, map[string]any{"id": s["id"], "arrivalDate": s["arrival"], "departureDate": s["departure"],
					"hasRejectedDates": false, "presenceDatesEdited": false, "visitingCustomer": s["visitingCustomer"]})
			}
		}
		writeAny(w, 200, map[string]any{"total": len(future), "pageTotal": len(future), "data": future})
	case r.Method == "GET" && strings.HasPrefix(path, "/plowing/customerPresences/"+testCabinID+"/"):
		id := strings.TrimPrefix(path, "/plowing/customerPresences/"+testCabinID+"/")
		for _, s := range f.stays {
			if s["id"] == id {
				writeAny(w, 200, s)
				return
			}
		}
		writeAny(w, 404, map[string]any{"code": "stay_not_found", "message": "Stay not found"})
	case r.Method == "POST" && path == "/plowing/customerPresences":
		needVersion("2.0")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.created = append(f.created, body)
		f.stays = append([]map[string]any{{
			"id": "88888888-8888-4888-8888-888888888888", "cabinId": body["cabinId"], "_future": true, "arrival": body["arrival"], "departure": body["departure"],
			"plowingGuaranteed": true, "rejectedDates": body["rejectedDays"], "createdAt": time.Now().UTC().Format(time.RFC3339),
			"visitingCustomer": map[string]any{"id": body["visitingCustomerId"], "firstName": "Kari", "lastName": "private-lastname"},
		}}, f.stays...)
		w.WriteHeader(201)
	case r.Method == "PUT" && strings.HasPrefix(path, "/plowing/customerPresences/"):
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		id := strings.TrimPrefix(path, "/plowing/customerPresences/")
		f.updated[id] = body
		for _, s := range f.stays {
			if s["id"] == id {
				s["arrival"], s["departure"], s["rejectedDates"] = body["arrival"], body["departure"], body["rejectedDays"]
			}
		}
		w.WriteHeader(204)
	case r.Method == "DELETE" && strings.HasPrefix(path, "/plowing/customerPresences/"):
		f.deleted = append(f.deleted, strings.TrimPrefix(path, "/plowing/customerPresences/"))
		w.WriteHeader(204)
	case r.Method == "GET" && path == "/plowing/cabin/"+testCabinID+"/history":
		writeAny(w, 200, map[string]any{"total": 1, "pageTotal": 1, "data": []map[string]any{{
			"eventDate": "2026-02-21T19:36:33.000Z", "plowingEventType": "Plow", "clipConsumed": 0,
			"user": map[string]any{"fullName": "private-driver", "phoneNumber": "private-driver-phone", "email": "private-driver@example.test"},
		}}})
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		writeAny(w, 404, map[string]any{"code": "not_found"})
	}
}

// newTestApp wires an app to the fake API with synthetic settings.
func newTestApp(t *testing.T, f *fakeFribo, readOnly bool) *app {
	t.Setenv("FRIBO_EMAIL", testEmail)
	t.Setenv("FRIBO_PASSWORD", testPassword)
	t.Setenv("FRIBO_API_URL", f.srv.URL)
	t.Setenv("FRIBO_CABIN_ID", "")
	if readOnly {
		t.Setenv("MCP_READ_ONLY", "true")
	} else {
		t.Setenv("MCP_READ_ONLY", "false")
	}
	t.Setenv("MCP_AUTH_TOKEN", "test-token")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newApp(log, openStore(t.TempDir(), log))
}

func assertNoPrivate(t *testing.T, label string, data []byte) {
	t.Helper()
	if i := strings.Index(string(data), "private-"); i >= 0 {
		end := min(len(data), i+40)
		t.Fatalf("%s leaks private data: %s", label, data[i:end])
	}
}
