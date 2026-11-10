//go:build mattn && cgo

package main

import (
	"database/sql"
	"net/url"

	"github.com/mattn/go-sqlite3"
)

func init() {
	sql.Register("sqlite3mattn", &sqlite3.SQLiteDriver{})
	drivers["mattn"] = func(path string, ro bool) string {
		dsn := "file:" + url.PathEscape(path)
		if ro {
			dsn += "?mode=ro"
		}
		return dsn
	}
}
