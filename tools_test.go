package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func fixedNow(t *testing.T, s string) {
	t.Helper()
	old := nowFunc
	nowFunc = func() time.Time { return at(s) }
	t.Cleanup(func() { nowFunc = old })
}

func connect(t *testing.T, a *app) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := a.server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// callTool returns the structured result, or the error text when the tool failed.
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	raw, _ := json.Marshal(res)
	assertNoPrivate(t, name, raw)
	if res.IsError {
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String()
	}
	data, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("%s: %v: %s", name, err, data)
	}
	return ""
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestReadToolsAgainstFakeFribo(t *testing.T) {
	fixedNow(t, "2026-11-20 09:00")
	f := newFakeFribo(t)
	cs := connect(t, newTestApp(t, f, true))

	if names := toolNames(t, cs); slices.Contains(names, "register_stay") || !slices.Contains(names, "plowing_status") {
		t.Fatalf("read-only tools: %v", names)
	}

	var status statusOut
	if msg := callTool(t, cs, "plowing_status", nil, &status); msg != "" {
		t.Fatal(msg)
	}
	if status.CabinID != testCabinID || status.Contract.Type != "PlowingContract" || !status.Season.Active {
		t.Fatalf("status: %+v", status)
	}
	if status.Deadlines.WorkDaysArrival != "by 11:00 the day before" || status.FirstPlowingHour == nil || *status.FirstPlowingHour != 5 {
		t.Fatalf("deadlines: %+v", status.Deadlines)
	}
	if status.EarliestGuaranteedArrival != "2026-11-22" {
		t.Fatalf("earliest: %s", status.EarliestGuaranteedArrival)
	}
	if len(status.UpcomingStays) != 1 || status.UpcomingStays[0].ID != testStayID || status.UpcomingStays[0].Visitor != "Kari" {
		t.Fatalf("upcoming: %+v", status.UpcomingStays)
	}
	if status.LastPlowed == nil || status.LastPlowed.Format(dateLayout) != "2026-02-21" {
		t.Fatalf("last plowed: %v", status.LastPlowed)
	}

	var stays staysOut
	callTool(t, cs, "list_stays", map[string]any{"include_past": true}, &stays)
	if stays.Count != 2 {
		t.Fatalf("stays: %+v", stays)
	}
	if got := stays.Stays[0].Arrival.Format(time.RFC3339); got != "2026-12-04T13:00:00+01:00" {
		t.Fatalf("stay times are not Norwegian time: %s", got)
	}

	var hist historyOut
	callTool(t, cs, "plowing_history", nil, &hist)
	if len(hist.Events) != 1 || hist.Events[0].Type != "Plow" {
		t.Fatalf("history: %+v", hist)
	}

	var check checkOut
	callTool(t, cs, "check_stay", map[string]any{"arrival_date": "2026-12-05", "arrival_hour": 13, "departure_date": "2026-12-07"}, &check)
	if len(check.Overlaps) != 1 || check.Assessment.PlowingGuaranteed {
		t.Fatalf("overlap not found: %+v", check)
	}
}

func TestOutOfSeasonWarning(t *testing.T) {
	fixedNow(t, "2026-11-20 09:00")
	f := newFakeFribo(t)
	f.seasonEnd = "2026-06-30T00:00:00.000Z"
	cs := connect(t, newTestApp(t, f, true))
	var status statusOut
	callTool(t, cs, "plowing_status", nil, &status)
	if status.Season.Active || len(status.Warnings) == 0 || !strings.Contains(status.Warnings[0], "outside") {
		t.Fatalf("want a season warning: %+v", status)
	}
}

func TestWriteTools(t *testing.T) {
	fixedNow(t, "2026-11-20 09:00")
	f := newFakeFribo(t)
	cs := connect(t, newTestApp(t, f, false))

	var reg registerOut
	if msg := callTool(t, cs, "register_stay", map[string]any{
		"arrival_date": "2026-12-18", "arrival_hour": 13, "departure_date": "2026-12-20", "skip_plowing_dates": []string{"2026-12-19"},
	}, &reg); msg != "" {
		t.Fatal(msg)
	}
	if !reg.Registered || reg.Stay == nil || !reg.Assessment.PlowingGuaranteed {
		t.Fatalf("register: %+v", reg)
	}
	want := map[string]any{"cabinId": testCabinID, "visitingCustomerId": testUserID, "arrival": "2026-12-18T12:00:00.000Z",
		"departure": "2026-12-20T22:59:59.000Z", "rejectedDays": []any{"2026-12-19"}, "comment": nil}
	if got := f.created[0]; !jsonEqual(got, want) {
		t.Fatalf("create body\n got %v\nwant %v", got, want)
	}

	var upd registerOut
	if msg := callTool(t, cs, "update_stay", map[string]any{"stay_id": testStayID, "departure_date": "2026-12-07"}, &upd); msg != "" {
		t.Fatal(msg)
	}
	body := f.updated[testStayID]
	if body["arrival"] != "2026-12-04T12:00:00.000Z" || body["departure"] != "2026-12-07T22:59:59.000Z" || body["cabinId"] != nil {
		t.Fatalf("update body: %v", body)
	}

	var cancel cancelOut
	callTool(t, cs, "cancel_stay", map[string]any{"stay_id": testStayID}, &cancel)
	if len(f.deleted) != 1 || f.deleted[0] != testStayID {
		t.Fatalf("delete: %v", f.deleted)
	}

	// Fribo's error codes are explained.
	if msg := callTool(t, cs, "update_stay", map[string]any{"stay_id": "99999999-9999-4999-8999-999999999999"}, &upd); !strings.Contains(msg, "no such stay") {
		t.Fatalf("want stay_not_found explained, got %q", msg)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestNotConfigured(t *testing.T) {
	for _, k := range []string{"FRIBO_EMAIL", "FRIBO_PASSWORD", "FRIBO_CABIN_ID", "FRIBO_API_URL"} {
		t.Setenv(k, "")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cs := connect(t, newApp(log, openStore(t.TempDir(), log)))
	var out cabinsOut
	if msg := callTool(t, cs, "list_cabins", nil, &out); !strings.Contains(msg, "not set up") {
		t.Fatalf("want a setup hint, got %q", msg)
	}
}

// Exercise the production handler with modern discovery and the legacy
// handshake, without HTTP sessions.
func TestHTTPDiscoveryAndTools(t *testing.T) {
	f := newFakeFribo(t)
	a := newTestApp(t, f, false)
	srv := httptest.NewServer(a.mcpHandler())
	defer srv.Close()
	call := func(method, version, params string, token string) (int, json.RawMessage) {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
		req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", version)
		req.Header.Set("Mcp-Method", method)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, nil
		}
		if resp.Header.Get("Mcp-Session-Id") != "" {
			t.Fatal("unexpected HTTP session")
		}
		if i := bytes.Index(data, []byte("data: ")); i >= 0 {
			data = bytes.SplitN(data[i+6:], []byte("\n"), 2)[0]
		}
		var rpc struct{ Result, Error json.RawMessage }
		if err := json.Unmarshal(data, &rpc); err != nil || len(rpc.Error) > 0 {
			t.Fatalf("%s: %v %s", method, err, data)
		}
		return resp.StatusCode, rpc.Result
	}
	if code, _ := call("tools/list", "2025-11-25", `{}`, "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("want 401 without the right token, got %d", code)
	}
	modernMeta := `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"test-client","version":"1.0"},"io.modelcontextprotocol/clientCapabilities":{}}}`
	_, res := call("server/discover", "2026-07-28", modernMeta, "test-token")
	var discovery struct {
		SupportedVersions []string
	}
	json.Unmarshal(res, &discovery)
	if !slices.Contains(discovery.SupportedVersions, "2026-07-28") {
		t.Fatalf("unexpected discovery: %s", res)
	}
	for _, version := range []string{"2026-07-28", "2025-11-25"} {
		params := modernMeta
		if version == "2025-11-25" {
			call("initialize", version, `{"protocolVersion":"2025-11-25","clientInfo":{"name":"legacy-client","version":"1.0"},"capabilities":{}}`, "test-token")
			params = `{}`
		}
		_, res := call("tools/list", version, params, "test-token")
		var tools struct{ Tools []struct{ Name string } }
		json.Unmarshal(res, &tools)
		var names []string
		for _, tool := range tools.Tools {
			names = append(names, tool.Name)
		}
		for _, want := range []string{"plowing_status", "register_stay", "cancel_stay"} {
			if !slices.Contains(names, want) {
				t.Fatalf("%s: %s missing from %v", version, want, names)
			}
		}
	}
}

func TestConfigAPI(t *testing.T) {
	f := newFakeFribo(t)
	a := newTestApp(t, f, true)
	// Let the password and read-only flag come from the settings API instead.
	a.envSet["password"], a.envSet["read_only"] = false, false
	a.env.Password = ""
	mux := http.NewServeMux()
	a.registerConfigAPI(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	do := func(method, path, body string) (int, []byte) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, data
	}
	if code, _ := do("POST", "/config", `{"values":{"FRIBO_PASSWORD":"`+testPassword+`","MCP_READ_ONLY":"false"}}`); code != 200 {
		t.Fatalf("save: %d", code)
	}
	code, data := do("GET", "/config", "")
	if code != 200 {
		t.Fatalf("config: %d", code)
	}
	assertNoPrivate(t, "config", data)
	var doc configDoc
	json.Unmarshal(data, &doc)
	byKey := map[string]configField{}
	for _, f := range doc.Fields {
		byKey[f.Key] = f
	}
	if !doc.Configured || !byKey["FRIBO_PASSWORD"].Set || byKey["FRIBO_PASSWORD"].Value != "" || !byKey["FRIBO_EMAIL"].Locked {
		t.Fatalf("config doc: %s", data)
	}
	if opts := byKey["FRIBO_CABIN_ID"].Options; len(opts) != 1 || opts[0].Value != testCabinID {
		t.Fatalf("cabin options: %+v", opts)
	}
	if code, _ := do("POST", "/config", `{"values":{"FRIBO_EMAIL":"other@example.test"}}`); code != 400 {
		t.Fatalf("env-locked setting changed: %d", code)
	}
	if a.current().ReadOnly {
		t.Fatal("read_only from the API was not applied")
	}

	code, data = do("GET", "/healthz?check=1", "")
	if code != 200 || !bytes.Contains(data, []byte(`"ok":true`)) {
		t.Fatalf("check: %d %s", code, data)
	}
	assertNoPrivate(t, "health", data)
}
