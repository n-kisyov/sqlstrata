package app

import (
	"path/filepath"
	"testing"
)

func TestSessionRoundTripAndMRU(t *testing.T) {
	p := filepath.Join(t.TempDir(), "SQLStrata", "session.json")
	s, err := LoadSession(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.RecentFiles) != 0 {
		t.Fatalf("expected empty initial recent files")
	}

	s.ThemeSet = true
	s.DarkTheme = true
	s.AddRecent(`C:\data\one.db`)
	s.AddRecent(`C:\data\two.db`)
	s.AddRecent(`c:\data\ONE.db`) // moves to front case-insensitively

	if len(s.RecentFiles) != 2 || s.RecentFiles[0] != `c:\data\ONE.db` {
		t.Fatalf("unexpected MRU recent files: %v", s.RecentFiles)
	}

	s.AddQuery("SELECT 1;")
	s.AddQuery("SELECT 2;")
	s.AddQuery("SELECT 1;")
	if len(s.QueryHistory) != 2 || s.QueryHistory[0] != "SELECT 1;" {
		t.Fatalf("unexpected MRU query history: %v", s.QueryHistory)
	}

	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSession(p)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.ThemeSet || !loaded.DarkTheme || len(loaded.RecentFiles) != 2 || len(loaded.QueryHistory) != 2 {
		t.Fatalf("session round-trip mismatch: %+v", loaded)
	}
}

