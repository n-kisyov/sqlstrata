package main

import (
	"net/url"

	_ "github.com/ncruces/go-sqlite3/driver"
)

func init() {
	drivers["ncruces"] = func(path string, ro bool) string {
		dsn := "file:" + url.PathEscape(path)
		if ro {
			dsn += "?mode=ro"
		}
		return dsn
	}
}

