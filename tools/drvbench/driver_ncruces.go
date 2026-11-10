package main

import (
	"path/filepath"
	"strings"

	_ "github.com/ncruces/go-sqlite3/driver"
)

func init() {
	drivers["ncruces"] = func(path string, ro bool) string {
		esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.ToSlash(path))
		dsn := "file:" + esc
		if ro {
			dsn += "?mode=ro"
		}
		return dsn
	}
}

