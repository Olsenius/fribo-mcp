package main

// The gateway contract: endpoints a gateway (github.com/Olsenius/mcp-gateway)
// uses to show a settings form and a test button for this server.
//
//   GET  /healthz            liveness, no auth
//   GET  /healthz?check=1    logs in to Fribo and reads the cabin and contract
//   GET  /config             the settings: schema, values (never secrets), lock state
//   POST /config             {"values": {"ENV_NAME": "value"}, "clear": ["ENV_NAME"]}
//
// All but plain /healthz need "Authorization: Bearer <CONFIG_TOKEN>".

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type fieldOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type configField struct {
	Key      string        `json:"key"`
	Label    string        `json:"label"`
	Type     string        `json:"type"` // text, url, secret, bool, select
	Required bool          `json:"required,omitempty"`
	Help     string        `json:"help,omitempty"`
	Value    string        `json:"value"`
	Set      bool          `json:"set"`
	Locked   bool          `json:"locked"` // set by an environment variable
	Options  []fieldOption `json:"options,omitempty"`
	Default  string        `json:"default,omitempty"`
}

type configDoc struct {
	Name       string        `json:"name"`
	Version    string        `json:"version"`
	Configured bool          `json:"configured"`
	Fields     []configField `json:"fields"`
}

type checkResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type healthDoc struct {
	OK     bool          `json:"ok"`
	Status string        `json:"status"`
	Checks []checkResult `json:"checks,omitempty"`
}

var fieldDefs = []struct {
	key, label, typ, help, def string
	required                   bool
}{
	{"email", "E-mail", "text", "The e-mail you log in to portal.hyttetjenester.no with.", "", true},
	{"password", "Password", "secret", "Stored on the server and never shown again.", "", true},
	{"cabin_id", "Cabin", "select", "Empty = the account's default cabin.", "", false},
	{"read_only", "Read-only", "bool", "Hides the tools that register, change and cancel stays.", "", false},
	{"api_url", "Fribo API URL", "url", "Normally no need to change.", defaultAPIURL, false},
}

func settingValue(s Settings, key string) string {
	switch key {
	case "email":
		return s.Email
	case "password":
		return s.Password
	case "cabin_id":
		return s.CabinID
	case "api_url":
		return s.APIURL
	case "read_only":
		if s.ReadOnly {
			return "true"
		}
		return "false"
	}
	return ""
}

func setSetting(s *Settings, key, v string) error {
	v = strings.TrimSpace(v)
	switch key {
	case "email":
		s.Email = v
	case "password":
		s.Password = v
	case "cabin_id":
		s.CabinID = v
	case "api_url":
		if v != "" && !strings.HasPrefix(v, "https://") && !strings.HasPrefix(v, "http://") {
			return fmt.Errorf("%s must start with https://", envNames[key])
		}
		s.APIURL = strings.TrimRight(v, "/")
	case "read_only":
		s.ReadOnly = parseBool(v)
	default:
		return fmt.Errorf("unknown setting %q", key)
	}
	return nil
}

func keyForEnv(env string) string {
	for k, e := range envNames {
		if strings.EqualFold(e, env) {
			return k
		}
	}
	return ""
}

func (a *app) cabinOptions(ctx context.Context) []fieldOption {
	c, _, err := a.client()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cabins, err := c.Cabins(ctx)
	if err != nil {
		return nil
	}
	var out []fieldOption
	for _, cb := range cabins {
		label := cb.Name
		if cb.IsDefault {
			label += " (default)"
		}
		out = append(out, fieldOption{Value: cb.ID, Label: label})
	}
	return out
}

func (a *app) configDoc(ctx context.Context) configDoc {
	s := a.current()
	var stored Settings
	a.store.read(func(st *state) { stored = st.Settings })
	doc := configDoc{Name: "fribo-mcp", Version: version, Configured: s.ready()}
	for _, d := range fieldDefs {
		f := configField{Key: envNames[d.key], Label: d.label, Type: d.typ, Help: d.help, Required: d.required, Default: d.def, Locked: a.envSet[d.key]}
		v := settingValue(s, d.key)
		f.Set = v != ""
		switch d.typ {
		case "bool":
			f.Set = a.envSet[d.key] || stored.ReadOnly
		case "secret":
			v = ""
		}
		f.Value = v
		if d.key == "cabin_id" {
			f.Options = a.cabinOptions(ctx)
		}
		doc.Fields = append(doc.Fields, f)
	}
	return doc
}

type configUpdate struct {
	Values map[string]string `json:"values"`
	Clear  []string          `json:"clear"`
}

func (a *app) applyConfig(u configUpdate) error {
	// Validate first, then store.
	var stored Settings
	a.store.read(func(st *state) { stored = st.Settings })
	next := stored
	for env, v := range u.Values {
		key := keyForEnv(env)
		if key == "" {
			return fmt.Errorf("unknown setting %q", env)
		}
		if a.envSet[key] {
			return fmt.Errorf("%s is set by an environment variable and cannot be changed here", env)
		}
		if key == "password" && v == "" {
			continue // empty secret = keep
		}
		if err := setSetting(&next, key, v); err != nil {
			return err
		}
	}
	for _, env := range u.Clear {
		key := keyForEnv(env)
		if key == "" {
			return fmt.Errorf("unknown setting %q", env)
		}
		if a.envSet[key] {
			return fmt.Errorf("%s is set by an environment variable and cannot be changed here", env)
		}
		setSetting(&next, key, "")
	}
	if err := a.store.update(func(st *state) { st.Settings = next }); err != nil {
		return err
	}
	a.reload()
	return nil
}

func (a *app) runChecks(ctx context.Context) healthDoc {
	h := healthDoc{OK: true, Status: "ok"}
	add := func(name string, err error, detail string) {
		c := checkResult{Name: name, OK: err == nil, Detail: detail}
		if err != nil {
			c.Detail = err.Error()
			h.OK = false
		}
		h.Checks = append(h.Checks, c)
	}
	c, _, err := a.client()
	if err != nil {
		add("Settings", err, "")
		h.Status = "not configured"
		return h
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	cabins, err := c.Cabins(ctx)
	add("Login to Fribo", err, fmt.Sprintf("logged in, %d cabin(s)", len(cabins)))
	if err == nil {
		cc, cerr := a.cabinContext(ctx, "")
		detail := ""
		if cerr == nil {
			detail = cc.contract.Type + ", " + cc.contract.Status
		}
		add("Plowing contract", cerr, detail)
	}
	if !h.OK {
		h.Status = "failing"
	}
	return h
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (a *app) configAuthorized(r *http.Request) bool {
	if a.configToken == "" {
		return false
	}
	return constEq(r.Header.Get("Authorization"), "Bearer "+a.configToken)
}

func (a *app) registerConfigAPI(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("check") == "" {
			w.Write([]byte("ok"))
			return
		}
		if !a.configAuthorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		h := a.runChecks(r.Context())
		code := http.StatusOK
		if !h.OK {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, h)
	})

	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		if a.configToken == "" {
			http.NotFound(w, r)
			return
		}
		if !a.configAuthorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, a.configDoc(r.Context()))
		case http.MethodPost:
			var u configUpdate
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&u); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
				return
			}
			if err := a.applyConfig(u); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			a.log.Info("settings updated via /config")
			writeJSON(w, http.StatusOK, a.configDoc(r.Context()))
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
