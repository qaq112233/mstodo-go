package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qaq112233/mstodo-go/internal/store"
)

func TestInitializationPersistenceAndEpochFences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "mstodo.db")
	s, fresh, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Fatal("not initialized")
	}
	v, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Settings.AdminSecret) != 64 || len(v.Settings.Bearer) != 64 || len(v.Settings.PathSecret) != 64 || v.Settings.AuthMode != "bearer" {
		t.Fatal("weak/missing credentials")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm()&0077 != 0 {
		t.Fatal("database not private", info.Mode())
	}
	var journal string
	_ = s.DB.QueryRow("PRAGMA journal_mode").Scan(&journal)
	if journal != "wal" {
		t.Fatal("WAL missing")
	}
	now := time.Now().UTC()
	a := store.Account{ID: "owner", Refresh: "refresh", Maintained: now}
	if e := s.InstallAccount(context.Background(), v.Epoch, a); e != nil {
		t.Fatal(e)
	}
	v, err = s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if e := s.PutCache(context.Background(), v, "test", map[string]any{"tasks": []any{}}, now); e != nil {
		t.Fatal(e)
	}
	if e := s.DB.Close(); e != nil {
		t.Fatal(e)
	}
	s, fresh, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	if fresh {
		t.Fatal("recreated credentials")
	}
	persisted, _ := s.Snapshot(context.Background())
	if persisted.Settings.AdminSecret != v.Settings.AdminSecret || persisted.Settings.Bearer != v.Settings.Bearer {
		t.Fatal("credentials lost")
	}
	restored, e := s.Account(context.Background(), persisted.Epoch)
	if e != nil || restored.Refresh != "refresh" {
		t.Fatal("auth not persisted", e)
	}
	cfg := persisted.Settings
	cfg.ExpectedAccount = "other@example.test"
	if e = s.SaveSettings(context.Background(), persisted, cfg); e != nil {
		t.Fatal(e)
	}
	if e = s.PutCache(context.Background(), v, "old", nil, now); e != store.ErrChanged {
		t.Fatal("old request not fenced", e)
	}
	if e = s.SaveAccount(context.Background(), v.Epoch, a); e != store.ErrChanged {
		t.Fatal("old token not fenced", e)
	}
	next, _ := s.Snapshot(context.Background())
	if _, e = s.Account(context.Background(), next.Epoch); e == nil {
		t.Fatal("old identity retained after settings change")
	}
}
