package main

import (
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

func init() {
	drivers["modernc"] = func(path string, ro bool) string {
		esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.ToSlash(path))
		dsn := "file:" + esc
		if ro {
			dsn += "?mode=ro"
		}
		return dsn
	}
}

