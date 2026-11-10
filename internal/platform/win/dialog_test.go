//go:build windows

package win

import "testing"

func TestEncodeFilter(t *testing.T) {
	got := encodeFilter([]Filter{
		{Name: "SQLite", Pattern: "*.db;*.sqlite"},
		{Name: "All", Pattern: "*.*"},
	})
	if len(got) < 4 || got[len(got)-1] != 0 || got[len(got)-2] != 0 {
		t.Fatalf("filter not double-NUL terminated: %v", got)
	}
}
