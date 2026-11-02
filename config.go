package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const defaultAPIURL = "https://portal.hyttetjenester.no/__platform"

// Settings are the values that can come from the gateway's settings page or
// from environment variables. Environment variables always win.
type Settings struct {
	Email    string `json:"email,omitempty"`
	Password string `json:"password,omitempty"`
	CabinID  string `json:"cabin_id,omitempty"`
	APIURL   string `json:"api_url,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// envNames maps each setting to its environment variable.
var envNames = map[string]string{
	"email":     "FRIBO_EMAIL",
	"password":  "FRIBO_PASSWORD",
	"cabin_id":  "FRIBO_CABIN_ID",
	"api_url":   "FRIBO_API_URL",
	"read_only": "MCP_READ_ONLY",
}

func parseBool(v string) bool {
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes") || strings.EqualFold(v, "on")
}

// fromEnv returns the settings given as environment variables and which of
// them were set.
func fromEnv() (Settings, map[string]bool) {
	set := map[string]bool{}
	get := func(key string) string {
		v := strings.TrimSpace(os.Getenv(envNames[key]))
		if v != "" {
			set[key] = true
		}
		return v
	}
	s := Settings{Email: get("email"), Password: get("password"), CabinID: get("cabin_id"), APIURL: get("api_url")}
	if v := get("read_only"); v != "" {
		s.ReadOnly = parseBool(v)
	}
	return s, set
}

// merge lays env on top of stored.
func merge(stored, env Settings, set map[string]bool) Settings {
	out := stored
	for key, pair := range map[string][2]*string{
		"email":    {&out.Email, &env.Email},
		"password": {&out.Password, &env.Password},
		"cabin_id": {&out.CabinID, &env.CabinID},
		"api_url":  {&out.APIURL, &env.APIURL},
	} {
		if set[key] {
			*pair[0] = *pair[1]
		}
	}
	if set["read_only"] {
		out.ReadOnly = env.ReadOnly
	}
	if out.APIURL == "" {
		out.APIURL = defaultAPIURL
	}
	out.APIURL = strings.TrimRight(out.APIURL, "/")
	return out
}

func (s Settings) ready() bool { return s.Email != "" && s.Password != "" }

// ---------- persisted state ----------

type state struct {
	Settings Settings `json:"settings"`
}

// store keeps the state in DATA_DIR/state.json (mode 0600). Without a
// writable directory everything lives in memory until restart. Only
// settings are stored: Fribo tokens are kept in memory.
type store struct {
	mu   sync.Mutex
	path string
	st   state
	log  *slog.Logger
}

func openStore(dir string, log *slog.Logger) *store {
	s := &store{log: log}
	if dir == "" {
		log.Warn("DATA_DIR is empty, settings are kept in memory only")
		return s
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Warn("data dir not usable, settings are kept in memory only", "dir", dir, "err", err)
		return s
	}
	s.path = filepath.Join(dir, "state.json")
	data, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		log.Warn("could not read state", "err", err)
	default:
		if err := json.Unmarshal(data, &s.st); err != nil {
			log.Warn("state file is corrupt, starting empty", "err", err)
		}
	}
	return s
}

// update changes the state under lock and writes it to disk.
func (s *store) update(fn func(*state)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	return os.Rename(tmp, s.path)
}

func (s *store) read(fn func(*state)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
}

func constEq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
