// Package app stores persisted SQLStrata application settings, recent
// database files, and SQL editor query history.
package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	maxRecentFiles  = 12
	maxQueryHistory = 30
)

// Session holds persisted UI preferences and history.
type Session struct {
	ThemeSet     bool     `json:"theme_set,omitempty"`
	DarkTheme    bool     `json:"dark_theme,omitempty"`
	RecentFiles  []string `json:"recent_files,omitempty"`
	QueryHistory []string `json:"query_history,omitempty"`
}

// DefaultSessionPath returns %APPDATA%\SQLStrata\session.json (or OS user
// config fallback).
func DefaultSessionPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "SQLStrata", "session.json"), nil
}

// LoadSession reads the session file at path. Missing files yield an empty
// Session with nil error.
func LoadSession(path string) (*Session, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Session{}, nil
		}
		return &Session{}, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return &Session{}, err
	}
	return &s, nil
}

// Save writes s atomically to path.
func (s *Session) Save(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// AddRecent records filePath at the front of RecentFiles (case-insensitive dedup).
func (s *Session) AddRecent(filePath string) {
	if abs, err := filepath.Abs(filePath); err == nil {
		filePath = abs
	}
	if filePath == "" {
		return
	}
	filtered := make([]string, 0, len(s.RecentFiles)+1)
	filtered = append(filtered, filePath)
	for _, existing := range s.RecentFiles {
		if !strings.EqualFold(existing, filePath) {
			filtered = append(filtered, existing)
		}
	}
	if len(filtered) > maxRecentFiles {
		filtered = filtered[:maxRecentFiles]
	}
	s.RecentFiles = filtered
}

// AddQuery records sql at the front of QueryHistory (deduped).
func (s *Session) AddQuery(sql string) {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return
	}
	filtered := make([]string, 0, len(s.QueryHistory)+1)
	filtered = append(filtered, sql)
	for _, q := range s.QueryHistory {
		if q != sql {
			filtered = append(filtered, q)
		}
	}
	if len(filtered) > maxQueryHistory {
		filtered = filtered[:maxQueryHistory]
	}
	s.QueryHistory = slices.Clip(filtered)
}

