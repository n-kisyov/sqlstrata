package main

import (
	"net/url"

	_ "modernc.org/sqlite"
)

func init() {
	drivers["modernc"] = func(path string, ro bool) string {
		dsn := "file:" + url.PathEscape(path)
		if ro {
			dsn += "?mode=ro"
		}
		return dsn
	}
}
