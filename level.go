package entcache

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/gob"
	"errors"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"

	"github.com/golang/groupcache/lru"
)

type (
	// Entry defines an entry to store in a cache.
	Entry struct {
		Columns []string
		Values  [][]driver.Value
	}

	// A Key defines a comparable Go value.
	// See http://golang.org/ref/spec#Comparison_operators
	Key interface{}

	// AddGetDeleter defines the interface for getting,
	// adding and deleting entries from the cache.
	AddGetDeleter interface {
		Del(context.Context, Key) error
		Add(context.Context, Key, *Entry, time.Duration) error
		Get(context.Context, Key) (*Entry, error)
	}
)

// ErrNotFound is returned by Get when and Entry does not exist in the cache.
var ErrNotFound = errors.New("entcache: entry was not found")

type (
	// LRU provides an LRU cache that implements the AddGetter interface.
	LRU struct {
		*lru.Cache
	}
	// entry wraps the Entry with additional expiry information.
	entry struct {
		*Entry
		expiry time.Time
	}
)

// New creates a new Cache.
// If maxEntries is zero, the cache has no limit.
func NewLRU(maxEntries int) *LRU {
	return &LRU{
		Cache: lru.New(maxEntries),
	}
}

// Add adds the entry to the cache.
func (l *LRU) Add(_ context.Context, k Key, e *Entry, ttl time.Duration) error {
	if ttl == 0 {
		l.Cache.Add(k, e)
	} else {
		l.Cache.Add(k, &entry{Entry: e, expiry: time.Now().Add(ttl)})
	}
	return nil
}

// Get gets an entry from the cache.
func (l *LRU) Get(_ context.Context, k Key) (*Entry, error) {
	e, ok := l.Cache.Get(k)
	if !ok {
		return nil, ErrNotFound
	}
	switch e := e.(type) {
	case *Entry:
		return e, nil
	case *entry:
		if time.Now().Before(e.expiry) {
			return e.Entry, nil
		}
		l.Cache.Remove(k)
		return nil, ErrNotFound
	default:
		return nil, fmt.Errorf("entcache: unexpected entry type: %T", e)
	}
}

// Del deletes an entry from the cache.
func (l *LRU) Del(_ context.Context, k Key) error {
	l.Cache.Remove(k)
	return nil
}

func init() {
	// Register non builtin driver.Values.
	gob.Register(time.Time{})
}

// Redis provides a remote cache backed by Redis
// and implements the SetGetter interface.
type Redis struct {
	c *redis.Client
}

// Add adds the entry to the cache.
func (r *Redis) Add(ctx context.Context, k Key, e *Entry, ttl time.Duration) error {
	key := fmt.Sprint(k)
	if key == "" {
		return nil
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(e); err != nil {
		return err
	}
	if err := r.c.Set(ctx, key, buf, ttl).Err(); err != nil {
		return err
	}
	return nil
}

// Get gets an entry from the cache.
func (r *Redis) Get(ctx context.Context, k Key) (*Entry, error) {
	key := fmt.Sprint(k)
	if key == "" {
		return nil, ErrNotFound
	}
	buf, err := r.c.Get(ctx, key).Bytes()
	if err != nil || len(buf) == 0 {
		return nil, ErrNotFound
	}
	var e *Entry
	if err := gob.NewDecoder(bytes.NewBuffer(buf)).Decode(e); err != nil {
		return nil, ErrNotFound
	}
	return e, nil
}

// Del deletes an entry from the cache.
func (r *Redis) Del(ctx context.Context, k Key) error {
	key := fmt.Sprint(k)
	if key == "" {
		return nil
	}
	return r.c.Del(ctx, key).Err()
}

// multiLevel provides a multi-level cache implementation.
type multiLevel struct {
	levels []AddGetDeleter
}

// Add adds the entry to the cache.
func (m *multiLevel) Add(ctx context.Context, k Key, e *Entry, ttl time.Duration) error {
	for i := range m.levels {
		if err := m.levels[i].Add(ctx, k, e, ttl); err != nil {
			return err
		}
	}
	return nil
}

// Get gets an entry from the cache.
func (m *multiLevel) Get(ctx context.Context, k Key) (*Entry, error) {
	for i := range m.levels {
		switch e, err := m.levels[i].Get(ctx, k); {
		case err == nil:
			return e, nil
		case err != ErrNotFound:
			return nil, err
		}
	}
	return nil, ErrNotFound
}

// Del deletes an entry from the cache.
func (m *multiLevel) Del(ctx context.Context, k Key) error {
	for i := range m.levels {
		if err := m.levels[i].Del(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

// contextLevel provides a context/request level cache implementation.
type contextLevel struct{}

// Get gets an entry from the cache.
func (*contextLevel) Get(ctx context.Context, k Key) (*Entry, error) {
	c, ok := FromContext(ctx)
	if !ok {
		return nil, ErrNotFound
	}
	return c.Get(ctx, k)
}

// Add adds the entry to the cache.
func (*contextLevel) Add(ctx context.Context, k Key, e *Entry, ttl time.Duration) error {
	c, ok := FromContext(ctx)
	if !ok {
		return nil
	}
	return c.Add(ctx, k, e, ttl)
}

// Del deletes an entry from the cache.
func (*contextLevel) Del(ctx context.Context, k Key) error {
	c, ok := FromContext(ctx)
	if !ok {
		return nil
	}
	return c.Del(ctx, k)
}
