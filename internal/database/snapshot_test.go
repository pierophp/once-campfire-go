package database

import (
	"context"
	"sync"
	"testing"
)

func TestMemoOnlyServesItsOwnVersion(t *testing.T) {
	var m versionMemo
	m.put(5, "k", 1)
	if v, ok := m.get(5, "k"); !ok || v != 1 {
		t.Fatalf("same version: %v %v", v, ok)
	}
	if _, ok := m.get(6, "k"); ok {
		t.Fatal("newer version reused an old entry")
	}
	m.put(6, "k", 2)
	if _, ok := m.get(5, "k"); ok {
		t.Fatal("older version read a newer memo")
	}
	m.put(5, "late", 3) // a request that began before the commit must not repopulate
	if _, ok := m.get(6, "late"); ok {
		t.Fatal("stale writer admitted")
	}
}

func TestMemoizedBypassesWithoutSnapshot(t *testing.T) {
	d := &DB{}
	calls := 0
	load := func() (int, error) { calls++; return calls, nil }
	for range 3 {
		memoized(d, context.Background(), "k", load)
	}
	if calls != 3 {
		t.Fatalf("calls = %d", calls)
	}
	ctx := WithSnapshot(context.Background(), &Snapshot{Version: 1, Valid: true})
	for range 3 {
		memoized(d, ctx, "k", load)
	}
	if calls != 4 {
		t.Fatalf("calls = %d, want one load for the snapshot", calls)
	}
}

func TestResponseVersionSeesCommitsAndSharesQueries(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	before, err := d.ResponseVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Write.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS version_probe(x)"); err != nil {
		t.Fatal(err)
	}
	after, err := d.ResponseVersion(ctx)
	if err != nil || after == before {
		t.Fatalf("commit not observed: %d -> %d (%v)", before, after, err)
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, err := d.ResponseVersion(ctx); err != nil || v != after {
				t.Errorf("got %d, %v", v, err)
			}
		}()
	}
	wg.Wait()
}
