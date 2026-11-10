// Package engine owns every SQLite connection used by SQLStrata.
//
// Each connection is driven by a dedicated worker goroutine. Callers submit
// closures that run on that goroutine, so a connection is never used
// concurrently and the UI goroutine never blocks on SQLite. Work can be
// cancelled through its context, which maps to sqlite3_interrupt.
package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ncruces/go-sqlite3"
)

// ErrClosed is returned when work is submitted to a closed connection.
var ErrClosed = errors.New("engine: connection closed")

// Options controls how a database is opened.
type Options struct {
	ReadOnly bool
	// CacheKiB is the per-connection page cache size (PRAGMA cache_size=-N).
	CacheKiB int
	// MaxMemory is the hard per-connection memory cap enforced by the driver.
	MaxMemory int64
	// MmapSize enables memory-mapped I/O when > 0 (PRAGMA mmap_size).
	MmapSize int64
}

// DefaultOptions are tuned for low, predictable memory use on huge files.
func DefaultOptions() Options {
	return Options{
		CacheKiB:  32 * 1024,
		MaxMemory: 512 << 20,
	}
}

// Conn is a single SQLite connection bound to a worker goroutine.
type Conn struct {
	name   string
	jobs   chan job
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex // guards closed and jobs sends
	closed bool
}

type job struct {
	ctx context.Context
	fn  func(*sqlite3.Conn) error
	res chan error
}

// openConn starts a worker goroutine and opens the database on it.
func openConn(name, path string, opt Options) (*Conn, error) {
	c := &Conn{name: name, jobs: make(chan job, 64), done: make(chan struct{})}
	ready := make(chan error, 1)
	go c.loop(path, opt, ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Conn) loop(path string, opt Options, ready chan<- error) {
	defer close(c.done)

	ctx := context.Background()
	if opt.MaxMemory > 0 {
		ctx = sqlite3.WithMaxMemory(ctx, opt.MaxMemory)
	}
	db, err := sqlite3.OpenContext(ctx, buildURI(path, opt.ReadOnly))
	if err == nil {
		err = configure(db, opt)
		if err != nil {
			db.Close()
		}
	}
	ready <- err
	if err != nil {
		return
	}
	defer db.Close()

	for j := range c.jobs {
		if err := j.ctx.Err(); err != nil {
			j.res <- err
			continue
		}
		old := db.SetInterrupt(j.ctx)
		err := runSafe(db, j.fn)
		db.SetInterrupt(old)
		j.res <- err
	}
}

func runSafe(db *sqlite3.Conn, fn func(*sqlite3.Conn) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("engine: panic in job: %v", r)
		}
	}()
	return fn(db)
}

func configure(db *sqlite3.Conn, opt Options) error {
	var b strings.Builder
	b.WriteString("PRAGMA busy_timeout=5000; PRAGMA temp_store=FILE;")
	if opt.CacheKiB > 0 {
		fmt.Fprintf(&b, "PRAGMA cache_size=-%d;", opt.CacheKiB)
	}
	if opt.MmapSize > 0 {
		fmt.Fprintf(&b, "PRAGMA mmap_size=%d;", opt.MmapSize)
	}
	return db.Exec(b.String())
}

func buildURI(path string, ro bool) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	// file:C:/dir/x.db?mode=ro. Only the characters that are special in an
	// SQLite URI path need escaping.
	esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.ToSlash(path))
	mode := "rwc"
	if ro {
		mode = "ro"
	}
	return "file:" + esc + "?mode=" + mode
}

// Do runs fn on the connection's goroutine and waits for it to finish.
// Cancelling ctx interrupts the running statement.
func (c *Conn) Do(ctx context.Context, fn func(*sqlite3.Conn) error) error {
	res := make(chan error, 1)
	if err := c.submit(job{ctx: ctx, fn: fn, res: res}); err != nil {
		return err
	}
	return <-res
}

// Go runs fn asynchronously and reports the result to done (if non-nil) from
// the worker goroutine.
func (c *Conn) Go(ctx context.Context, fn func(*sqlite3.Conn) error, done func(error)) {
	res := make(chan error, 1)
	if err := c.submit(job{ctx: ctx, fn: fn, res: res}); err != nil {
		if done != nil {
			done(err)
		}
		return
	}
	go func() {
		err := <-res
		if done != nil {
			done(err)
		}
	}()
}

func (c *Conn) submit(j job) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	c.jobs <- j
	return nil
}

// Close stops the worker after queued jobs drain and closes the connection.
func (c *Conn) Close() {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		close(c.jobs)
		c.mu.Unlock()
		<-c.done
	})
}

// DB is an open database file with connections split by role, so that long
// background work (counting, anchor building) never stalls interactive paging.
type DB struct {
	Path     string
	ReadOnly bool

	// Fg serves interactive, latency-sensitive reads (grid paging, schema).
	Fg *Conn
	// Bg serves long-running background reads (counts, anchors, checks).
	Bg *Conn
}

// Open opens path with the given options.
func Open(path string, opt Options) (*DB, error) {
	fg, err := openConn("fg", path, opt)
	if err != nil {
		return nil, err
	}
	bgOpt := opt
	bgOpt.ReadOnly = true // background work is read-only by construction
	bg, err := openConn("bg", path, bgOpt)
	if err != nil {
		fg.Close()
		return nil, err
	}
	return &DB{Path: path, ReadOnly: opt.ReadOnly, Fg: fg, Bg: bg}, nil
}

// Close closes all connections.
func (d *DB) Close() {
	d.Bg.Close()
	d.Fg.Close()
}
