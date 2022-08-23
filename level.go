package entcache

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/gob"
	"errors"
	"fmt"
	"sync"
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
	Key any

	// AddGetDeleter defines the interface for getting,
	// adding and deleting entries from the cache.
	AddGetDeleter interface {
		Del(context.Context, Key) error
		Add(context.Context, Key, *Entry, time.Duration) error
		Get(context.Context, Key) (*Entry, error)
	}
)

func init() {
	// Register non builtin driver.Values.
	gob.Register(time.Time{})
}

// MarshalBinary implements the encoding.BinaryMarshaler interface.
func (e Entry) MarshalBinary() ([]byte, error) {
	entry := struct {
		C []string
		V [][]driver.Value
	}{
		C: e.Columns,
		V: e.Values,
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(entry); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnmarshalBinary implements the encoding.BinaryUnmarshaler interface.
func (e *Entry) UnmarshalBinary(buf []byte) error {
	var entry struct {
		C []string
		V [][]driver.Value
	}
	if err := gob.NewDecoder(bytes.NewBuffer(buf)).Decode(&entry); err != nil {
		return err
	}
	e.Values = entry.V
	e.Columns = entry.C
	return nil
}

// ErrNotFound is returned by Get when and Entry does not exist in the cache.
var ErrNotFound = errors.New("entcache: entry was not found")

type (
	// LRU provides an LRU cache that implements the AddGetter interface.
	LRU struct {
		mu sync.RWMutex
		*lru.Cache
	}
	// entry wraps the Entry with additional expiry information.
	entry struct {
		*Entry
		expiry time.Time
	}
)

// NewLRU creates a new Cache.
// If maxEntries is zero, the cache has no limit.
func NewLRU(maxEntries int) *LRU {
	return &LRU{
		Cache: lru.New(maxEntries),
	}
}

// Add adds the entry to the cache.
func (l *LRU) Add(_ context.Context, k Key, e *Entry, ttl time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	buf, err := e.MarshalBinary()
	if err != nil {
		return err
	}
	ne := &Entry{}
	if err := ne.UnmarshalBinary(buf); err != nil {
		return err
	}
	if ttl == 0 {
		l.Cache.Add(k, ne)
	} else {
		l.Cache.Add(k, &entry{Entry: ne, expiry: time.Now().Add(ttl)})
	}
	return nil
}

// Get gets an entry from the cache.
func (l *LRU) Get(_ context.Context, k Key) (*Entry, error) {
	l.mu.RLock()
	e, ok := l.Cache.Get(k)
	l.mu.RUnlock()
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
		l.mu.Lock()
		l.Cache.Remove(k)
		l.mu.Unlock()
		return nil, ErrNotFound
	default:
		return nil, fmt.Errorf("entcache: unexpected entry type: %T", e)
	}
}

// Del deletes an entry from the cache.
func (l *LRU) Del(_ context.Context, k Key) error {
	l.mu.Lock()
	l.Cache.Remove(k)
	l.mu.Unlock()
	return nil
}

// Redis provides a remote cache backed by Redis
// and implements the SetGetter interface.
type Redis struct {
	c redis.Cmdable
}

// NewRedis returns a new Redis cache level from the given Redis connection.
//
//	entcache.NewRedis(redis.NewClient(&redis.Options{
//		Addr: ":6379"
//	}))
//
//	entcache.NewRedis(redis.NewClusterClient(&redis.ClusterOptions{
//		Addrs: []string{":7000", ":7001", ":7002"},
//	}))
func NewRedis(c redis.Cmdable) *Redis {
	return &Redis{c: c}
}

// Add adds the entry to the cache.
func (r *Redis) Add(ctx context.Context, k Key, e *Entry, ttl time.Duration) error {
	key := fmt.Sprint(k)
	if key == "" {
		return nil
	}
	buf, err := e.MarshalBinary()
	if err != nil {
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
	e := &Entry{}
	if err := e.UnmarshalBinary(buf); err != nil {
		return nil, err
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
