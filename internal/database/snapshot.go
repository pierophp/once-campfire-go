package database

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

type snapshotKey struct{}

// Snapshot records the database version a read-only request observed before it
// authenticated. While the version is unchanged, pure lookups (session, membership)
// return the same rows, so they are answered from memory instead of SQLite.
type Snapshot struct {
	Version uint64
	Valid   bool
}

func WithSnapshot(ctx context.Context, s *Snapshot) context.Context {
	return context.WithValue(ctx, snapshotKey{}, s)
}

func snapshotVersion(ctx context.Context) (uint64, bool) {
	s, _ := ctx.Value(snapshotKey{}).(*Snapshot)
	if s == nil || !s.Valid {
		return 0, false
	}
	return s.Version, true
}

const memoLimit = 4096

// versionMemo holds results for exactly one database version. A newer version
// discards it; an older request (one that began before a commit) bypasses it.
type versionMemo struct {
	mu      sync.RWMutex
	version uint64
	values  map[any]any
}

func (m *versionMemo) get(version uint64, key any) (any, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.version != version || m.values == nil {
		return nil, false
	}
	value, ok := m.values[key]
	return value, ok
}

func (m *versionMemo) put(version uint64, key, value any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case version < m.version:
		return
	case version > m.version || m.values == nil || len(m.values) >= memoLimit:
		m.version = version
		m.values = make(map[any]any)
	}
	m.values[key] = value
}

type sessionKey struct{ token string }
type activityKey struct{ token string }
type roomKey struct{ user, room int64 }

type memoEntry[T any] struct {
	value T
	err   error
}

// memoized runs load once per database version and key for requests that carry
// a valid snapshot; any other request calls load directly. Only definitive
// answers are retained: rows and "no such row".
func memoized[T any](d *DB, ctx context.Context, key any, load func() (T, error)) (T, error) {
	version, ok := snapshotVersion(ctx)
	if !ok {
		return load()
	}
	if cached, hit := d.memo.get(version, key); hit {
		entry := cached.(memoEntry[T])
		return entry.value, entry.err
	}
	value, err := load()
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		d.memo.put(version, key, memoEntry[T]{value, err})
	}
	return value, err
}
