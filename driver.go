package entcache

import (
	"context"
	stdsql "database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	_ "unsafe"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/mitchellh/hashstructure/v2"
)

type (
	// Options wraps the basic configuration cache options.
	Options struct {
		// TTL defines the period of time that an Entry
		// is valid in the cache.
		TTL time.Duration

		// Cache defines the GetAddDeleter (cache implementation)
		// for holding the cache entries. If no cache implementation
		// was provided, an LRU cache with no limit is used.
		Cache AddGetDeleter

		// Hash defines an optional Hash function for converting
		// a query and its arguments to a cache key. If no Hash
		// function was provided, the DefaultHash is used.
		Hash func(query string, args []any) (Key, error)

		// Logf function. If provided, the Driver will call it with
		// errors that can not be handled.
		Log func(...any)
	}

	// Option allows configuring the cache
	// driver using functional options.
	Option func(*Options)

	// A Driver is an SQL cached client. Users should use the
	// constructor below for creating new driver.
	Driver struct {
		dialect.Driver
		*Options
		stats Stats
	}
)

// NewDriver returns a new Driver an existing driver and optional
// configuration functions. For example:
//
//	entcache.NewDriver(
//		drv,
//		entcache.TTL(time.Minute),
//		entcache.Levels(
//			NewLRU(256),
//			NewRedis(redis.NewClient(&redis.Options{
//				Addr: ":6379",
//			})),
//		)
//	)
func NewDriver(drv dialect.Driver, opts ...Option) *Driver {
	options := &Options{Hash: DefaultHash, Cache: NewLRU(0)}
	for _, opt := range opts {
		opt(options)
	}
	return &Driver{
		Driver:  drv,
		Options: options,
	}
}

// TTL configures the period of time that an Entry
// is valid in the cache.
func TTL(ttl time.Duration) Option {
	return func(o *Options) {
		o.TTL = ttl
	}
}

// Hash configures an optional Hash function for
// converting a query and its arguments to a cache key.
func Hash(hash func(query string, args []any) (Key, error)) Option {
	return func(o *Options) {
		o.Hash = hash
	}
}

// Levels configures the Driver to work with the given cache levels.
// For example, in process LRU cache and a remote Redis cache.
func Levels(levels ...AddGetDeleter) Option {
	return func(o *Options) {
		if len(levels) == 1 {
			o.Cache = levels[0]
		} else {
			o.Cache = &multiLevel{levels: levels}
		}
	}
}

// ContextLevel configures the driver to work with context/request level cache.
// Users that use this option, should wraps the *http.Request context with the
// cache value as follows:
//
//	ctx = entcache.NewContext(ctx)
//
//	ctx = entcache.NewContext(ctx, entcache.NewLRU(128))
func ContextLevel() Option {
	return func(o *Options) {
		o.Cache = &contextLevel{}
	}
}

// Query implements the Querier interface for the driver. It falls back to the
// underlying wrapped driver in case of caching error.
//
// Note that, the driver does not synchronize identical queries that are executed
// concurrently. Hence, if 2 identical queries are executed at the ~same time, and
// there is no cache entry for them, the driver will execute both of them and the
// last successful one will be stored in the cache.
func (d *Driver) Query(ctx context.Context, query string, args, v any) error {
	// Check if the given statement looks like a standard Ent query (e.g. SELECT).
	// Custom queries (e.g. CTE) or statements that are prefixed with comments are
	// not supported. This check is mainly necessary, because PostgreSQL and SQLite
	// may execute insert statement like "INSERT ... RETURNING" using Driver.Query.
	if !strings.HasPrefix(query, "SELECT") && !strings.HasPrefix(query, "select") {
		return d.Driver.Query(ctx, query, args, v)
	}
	vr, ok := v.(*sql.Rows)
	if !ok {
		return fmt.Errorf("entcache: invalid type %T. expect *sql.Rows", v)
	}
	argv, ok := args.([]any)
	if !ok {
		return fmt.Errorf("entcache: invalid type %T. expect []interface{} for args", args)
	}
	opts, err := d.optionsFromContext(ctx, query, argv)
	if err != nil {
		return d.Driver.Query(ctx, query, args, v)
	}
	atomic.AddUint64(&d.stats.Gets, 1)
	switch e, err := d.Cache.Get(ctx, opts.key); {
	case err == nil:
		atomic.AddUint64(&d.stats.Hits, 1)
		vr.ColumnScanner = &repeater{columns: e.Columns, values: e.Values}
	case err == ErrNotFound:
		if err := d.Driver.Query(ctx, query, args, vr); err != nil {
			return err
		}
		vr.ColumnScanner = &recorder{
			ColumnScanner: vr.ColumnScanner,
			onClose: func(columns []string, values [][]driver.Value) {
				err := d.Cache.Add(ctx, opts.key, &Entry{Columns: columns, Values: values}, opts.ttl)
				if err != nil && d.Log != nil {
					atomic.AddUint64(&d.stats.Errors, 1)
					d.Log(fmt.Sprintf("entcache: failed storing entry %v in cache: %v", opts.key, err))
				}
			},
		}
	default:
		return d.Driver.Query(ctx, query, args, v)
	}
	return nil
}

// Stats returns a copy of the cache statistics.
func (d *Driver) Stats() Stats {
	return Stats{
		Gets:   atomic.LoadUint64(&d.stats.Gets),
		Hits:   atomic.LoadUint64(&d.stats.Hits),
		Errors: atomic.LoadUint64(&d.stats.Errors),
	}
}

// QueryContext calls QueryContext of the underlying driver, or fails if it is not supported.
// Note, this method is not part of the caching layer since Ent does not use it by default.
func (d *Driver) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	drv, ok := d.Driver.(interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	})
	if !ok {
		return nil, fmt.Errorf("Driver.QueryContext is not supported")
	}
	return drv.QueryContext(ctx, query, args...)
}

// ExecContext calls ExecContext of the underlying driver, or fails if it is not supported.
func (d *Driver) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	drv, ok := d.Driver.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	if !ok {
		return nil, fmt.Errorf("Driver.ExecContext is not supported")
	}
	return drv.ExecContext(ctx, query, args...)
}

// errSkip tells the driver to skip cache layer.
var errSkip = errors.New("entcache: skip cache")

// optionsFromContext returns the injected options from the context, or its default value.
func (d *Driver) optionsFromContext(ctx context.Context, query string, args []any) (ctxOptions, error) {
	var opts ctxOptions
	if c, ok := ctx.Value(ctxOptionsKey).(*ctxOptions); ok {
		opts = *c
	}
	if opts.key == nil {
		key, err := d.Hash(query, args)
		if err != nil {
			return opts, errSkip
		}
		opts.key = key
	}
	if opts.ttl == 0 {
		opts.ttl = d.TTL
	}
	if opts.evict {
		if err := d.Cache.Del(ctx, opts.key); err != nil {
			return opts, err
		}
	}
	if opts.skip {
		return opts, errSkip
	}
	return opts, nil
}

// DefaultHash provides the default implementation for converting
// a query and its argument to a cache key.
func DefaultHash(query string, args []any) (Key, error) {
	key, err := hashstructure.Hash(struct {
		Q string
		A []any
	}{
		Q: query,
		A: args,
	}, hashstructure.FormatV2, nil)
	if err != nil {
		return nil, err
	}
	return key, nil
}

// Stats represents the cache statistics of the driver.
type Stats struct {
	Gets   uint64
	Hits   uint64
	Errors uint64
}

// rawCopy copies the driver values by implementing
// the sql.Scanner interface.
type rawCopy struct {
	values []driver.Value
}

func (c *rawCopy) Scan(src interface{}) error {
	if b, ok := src.([]byte); ok {
		b1 := make([]byte, len(b))
		copy(b1, b)
		src = b1
	}
	c.values[0] = src
	c.values = c.values[1:]
	return nil
}

// recorder represents an sql.Rows recorder that implements
// the entgo.io/ent/dialect/sql.ColumnScanner interface.
type recorder struct {
	sql.ColumnScanner
	values  [][]driver.Value
	columns []string
	done    bool
	onClose func([]string, [][]driver.Value)
}

// Next wraps the underlying Next method
func (r *recorder) Next() bool {
	hasNext := r.ColumnScanner.Next()
	r.done = !hasNext
	return hasNext
}

// Scan copies database values for future use (by the repeater)
// and assign them to the given destinations using the standard
// database/sql.convertAssign function.
func (r *recorder) Scan(dest ...any) error {
	values := make([]driver.Value, len(dest))
	args := make([]any, len(dest))
	c := &rawCopy{values: values}
	for i := range args {
		args[i] = c
	}
	if err := r.ColumnScanner.Scan(args...); err != nil {
		return err
	}
	for i := range values {
		if err := convertAssign(dest[i], values[i]); err != nil {
			return err
		}
	}
	r.values = append(r.values, values)
	return nil
}

// Columns wraps the underlying Column method and stores it in the recorder state.
// The repeater.Columns cannot be called if the recorder method was not called before.
// That means, raw scanning should be identical for identical queries.
func (r *recorder) Columns() ([]string, error) {
	columns, err := r.ColumnScanner.Columns()
	if err != nil {
		return nil, err
	}
	r.columns = columns
	return columns, nil
}

func (r *recorder) Close() error {
	if err := r.ColumnScanner.Close(); err != nil {
		return err
	}
	// If we did not encounter any error during iteration,
	// and we scanned all rows, we store it on cache.
	if err := r.ColumnScanner.Err(); err == nil || r.done {
		r.onClose(r.columns, r.values)
	}
	return nil
}

// repeater repeats columns scanning from cache history.
type repeater struct {
	columns []string
	values  [][]driver.Value
}

func (*repeater) Close() error {
	return nil
}
func (*repeater) ColumnTypes() ([]*stdsql.ColumnType, error) {
	return nil, fmt.Errorf("entcache.ColumnTypes is not supported")
}
func (r *repeater) Columns() ([]string, error) {
	return r.columns, nil
}
func (*repeater) Err() error {
	return nil
}
func (r *repeater) Next() bool {
	return len(r.values) > 0
}
func (r *repeater) NextResultSet() bool {
	return len(r.values) > 0
}

func (r *repeater) Scan(dest ...any) error {
	if !r.Next() {
		return stdsql.ErrNoRows
	}
	for i, src := range r.values[0] {
		if err := convertAssign(dest[i], src); err != nil {
			return err
		}
	}
	r.values = r.values[1:]
	return nil
}

//go:linkname convertAssign database/sql.convertAssign
func convertAssign(dest, src any) error
