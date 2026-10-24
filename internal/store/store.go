// Package store owns the single local SQLite database. Network requests never run
// inside its transactions. Epoch fences isolate authorization generations;
// revision fences prevent an older read from repopulating a cache after a write.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var ErrChanged = errors.New("account_or_data_changed")

type Settings struct {
	Timezone        string `json:"timezone"`
	AuthMode        string `json:"authMode"`
	Bearer          string `json:"bearer"`
	PathSecret      string `json:"pathSecret"`
	AdminSecret     string `json:"adminSecret"`
	CSRF            string `json:"csrf"`
	Tenant          string `json:"tenant"`
	ClientID        string `json:"clientId"`
	ClientSecret    string `json:"clientSecret"`
	ExpectedAccount string `json:"expectedAccount"`
	MyDayMaxPages   int    `json:"myDayMaxPages"`
}

type Token struct {
	Access  string    `json:"access"`
	Expires time.Time `json:"expires"`
}
type Account struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Email            string    `json:"email"`
	Anchor           string    `json:"anchor"`
	Refresh          string    `json:"refresh"`
	Graph            Token     `json:"graph"`
	Substrate        Token     `json:"substrate"`
	Reauth           bool      `json:"reauth"`
	SubstrateConsent bool      `json:"substrateConsentRequired"`
	Maintained       time.Time `json:"maintained"`
	Attempted        time.Time `json:"attempted"`
	MaintenanceError bool      `json:"maintenanceError"`
}
type Snapshot struct {
	Settings Settings
	Epoch    int64
	Revision int64
}
type Cache struct {
	Data    json.RawMessage
	Fetched time.Time
	Expires time.Time
}
type SyncState struct {
	Cursor   string
	Complete bool
	Success  time.Time
	Error    string
}
type Store struct{ DB *sql.DB }

func Secret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func Open(path string) (*Store, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, false, err
	}
	// Create privately before opening SQLite (including on a permissive umask).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, err
	}
	f.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, false, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db}
	fail := func(e error) (*Store, bool, error) { db.Close(); return nil, false, e }
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA temp_store=MEMORY;`); err != nil {
		return fail(err)
	}
	var version int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fail(err)
	}
	if version > 1 {
		return fail(errors.New("database_version_newer_than_binary"))
	}
	if version == 0 {
		tx, e := db.Begin()
		if e != nil {
			return fail(e)
		}
		_, e = tx.Exec(`
CREATE TABLE settings(id INTEGER PRIMARY KEY CHECK(id=1), payload TEXT NOT NULL, epoch INTEGER NOT NULL, revision INTEGER NOT NULL);
CREATE TABLE account(id INTEGER PRIMARY KEY CHECK(id=1), epoch INTEGER NOT NULL, payload TEXT NOT NULL);
CREATE TABLE lists(epoch INTEGER NOT NULL,id TEXT NOT NULL,payload TEXT NOT NULL,PRIMARY KEY(epoch,id));
CREATE TABLE tasks(epoch INTEGER NOT NULL,list_id TEXT NOT NULL,id TEXT NOT NULL,payload TEXT NOT NULL,PRIMARY KEY(epoch,list_id,id));
CREATE TABLE sync_state(epoch INTEGER NOT NULL,list_id TEXT NOT NULL,cursor TEXT NOT NULL DEFAULT '',complete INTEGER NOT NULL DEFAULT 0,last_success_at INTEGER NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT '',PRIMARY KEY(epoch,list_id));
CREATE TABLE cache_entries(epoch INTEGER NOT NULL,key TEXT NOT NULL,payload TEXT NOT NULL,fetched_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,PRIMARY KEY(epoch,key));
PRAGMA user_version=1;`)
		if e != nil {
			tx.Rollback()
			return fail(e)
		}
		if e = tx.Commit(); e != nil {
			return fail(e)
		}
	}
	var n int
	if err = db.QueryRow(`SELECT count(*) FROM settings`).Scan(&n); err != nil {
		return fail(err)
	}
	fresh := n == 0
	if fresh {
		cfg := Settings{Timezone: "Asia/Shanghai", AuthMode: "bearer", Bearer: Secret(), PathSecret: Secret(), AdminSecret: Secret(), CSRF: Secret(), MyDayMaxPages: 30}
		b, _ := json.Marshal(cfg)
		if _, err = db.Exec(`INSERT INTO settings VALUES(1,?,1,0)`, string(b)); err != nil {
			return fail(err)
		}
	}
	return s, fresh, nil
}
func (s *Store) Snapshot(ctx context.Context) (Snapshot, error) {
	var v Snapshot
	var b string
	err := s.DB.QueryRowContext(ctx, `SELECT payload,epoch,revision FROM settings WHERE id=1`).Scan(&b, &v.Epoch, &v.Revision)
	if err == nil {
		err = json.Unmarshal([]byte(b), &v.Settings)
	}
	return v, err
}
func fence(ctx context.Context, tx *sql.Tx, epoch, revision int64) error {
	var e, r int64
	if err := tx.QueryRowContext(ctx, `SELECT epoch,revision FROM settings WHERE id=1`).Scan(&e, &r); err != nil {
		return err
	}
	if e != epoch || (revision >= 0 && r != revision) {
		return ErrChanged
	}
	return nil
}
func (s *Store) transaction(ctx context.Context, epoch, revision int64, fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fence(ctx, tx, epoch, revision); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func clear(tx *sql.Tx) error {
	for _, t := range []string{"account", "lists", "tasks", "sync_state", "cache_entries"} {
		if _, err := tx.Exec(`DELETE FROM ` + t); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) SaveSettings(ctx context.Context, old Snapshot, cfg Settings) error {
	changed := old.Settings.Tenant != cfg.Tenant || old.Settings.ClientID != cfg.ClientID || old.Settings.ClientSecret != cfg.ClientSecret || old.Settings.ExpectedAccount != cfg.ExpectedAccount
	b, _ := json.Marshal(cfg)
	return s.transaction(ctx, old.Epoch, old.Revision, func(tx *sql.Tx) error {
		epoch := old.Epoch
		if changed {
			epoch++
			if err := clear(tx); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`UPDATE settings SET payload=?,epoch=?,revision=revision+1 WHERE id=1`, string(b), epoch)
		return err
	})
}
func (s *Store) InstallAccount(ctx context.Context, epoch int64, a Account) error {
	b, _ := json.Marshal(a)
	return s.transaction(ctx, epoch, -1, func(tx *sql.Tx) error {
		if err := clear(tx); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE settings SET epoch=epoch+1,revision=revision+1 WHERE id=1`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO account VALUES(1,?,?)`, epoch+1, string(b))
		return err
	})
}
func (s *Store) Account(ctx context.Context, epoch int64) (Account, error) {
	var a Account
	var b string
	err := s.DB.QueryRowContext(ctx, `SELECT a.payload FROM account a JOIN settings s ON s.epoch=a.epoch WHERE a.id=1 AND a.epoch=?`, epoch).Scan(&b)
	if err == nil {
		err = json.Unmarshal([]byte(b), &a)
	}
	return a, err
}
func (s *Store) SaveAccount(ctx context.Context, epoch int64, a Account) error {
	b, _ := json.Marshal(a)
	return s.transaction(ctx, epoch, -1, func(tx *sql.Tx) error {
		r, e := tx.Exec(`UPDATE account SET payload=? WHERE id=1 AND epoch=?`, string(b), epoch)
		if e != nil {
			return e
		}
		n, _ := r.RowsAffected()
		if n != 1 {
			return ErrChanged
		}
		return nil
	})
}
func (s *Store) GetCache(ctx context.Context, epoch int64, key string) (Cache, error) {
	var c Cache
	var payload string
	var f, e int64
	err := s.DB.QueryRowContext(ctx, `SELECT c.payload,c.fetched_at,c.expires_at FROM cache_entries c JOIN settings s ON c.epoch=s.epoch WHERE c.epoch=? AND c.key=?`, epoch, key).Scan(&payload, &f, &e)
	c.Data = json.RawMessage(payload)
	c.Fetched = time.UnixMilli(f)
	c.Expires = time.UnixMilli(e)
	return c, err
}
func (s *Store) PutCache(ctx context.Context, v Snapshot, key string, data any, now time.Time) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return s.transaction(ctx, v.Epoch, v.Revision, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM cache_entries WHERE expires_at<?`, now.Add(-7*24*time.Hour).UnixMilli()); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT OR REPLACE INTO cache_entries VALUES(?,?,?,?,?)`, v.Epoch, key, string(b), now.UnixMilli(), now.Add(180*time.Second).UnixMilli())
		return err
	})
}

// Invalidate conservatively discards derived snapshots, without claiming that
// a successful task mutation refreshed a whole list. Page cursors remain resumable.
func (s *Store) Invalidate(ctx context.Context, epoch int64, lists []string, roster bool) error {
	return s.transaction(ctx, epoch, -1, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE settings SET revision=revision+1 WHERE id=1`); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM cache_entries WHERE epoch=?`, epoch); err != nil {
			return err
		}
		if roster {
			_, err := tx.Exec(`UPDATE sync_state SET last_success_at=0 WHERE epoch=?`, epoch)
			return err
		}
		for _, id := range lists {
			if _, err := tx.Exec(`UPDATE sync_state SET last_success_at=0 WHERE epoch=? AND list_id=?`, epoch, id); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Store) Lists(ctx context.Context, epoch int64) ([]map[string]any, error) {
	return s.rows(ctx, `SELECT payload FROM lists WHERE epoch=? ORDER BY id`, epoch)
}
func (s *Store) rows(ctx context.Context, q string, args ...any) ([]map[string]any, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var b string
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		var m map[string]any
		if err = json.Unmarshal([]byte(b), &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) SaveLists(ctx context.Context, v Snapshot, items []map[string]any, now time.Time) error {
	return s.transaction(ctx, v.Epoch, v.Revision, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM lists WHERE epoch=?`, v.Epoch); err != nil {
			return err
		}
		for _, m := range items {
			b, _ := json.Marshal(m)
			if _, err := tx.Exec(`INSERT INTO lists VALUES(?,?,?)`, v.Epoch, m["id"], string(b)); err != nil {
				return err
			}
		}
		// Roster replacement does not delete task data; removed scopes are never
		// included in an unscoped query and cannot erase an in-flight sync.
		b, _ := json.Marshal(items)
		_, err := tx.Exec(`INSERT OR REPLACE INTO cache_entries VALUES(?,?,?,?,?)`, v.Epoch, "lists", string(b), now.UnixMilli(), now.Add(180*time.Second).UnixMilli())
		return err
	})
}
func (s *Store) Sync(ctx context.Context, epoch int64, list string) (SyncState, error) {
	var v SyncState
	var ms int64
	err := s.DB.QueryRowContext(ctx, `SELECT cursor,complete,last_success_at,last_error FROM sync_state WHERE epoch=? AND list_id=?`, epoch, list).Scan(&v.Cursor, &v.Complete, &ms, &v.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	if ms > 0 {
		v.Success = time.UnixMilli(ms)
	}
	return v, err
}
func (s *Store) ResetSync(ctx context.Context, v Snapshot, list string) error {
	return s.transaction(ctx, v.Epoch, v.Revision, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM tasks WHERE epoch=? AND list_id=?`, v.Epoch, list); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT OR REPLACE INTO sync_state(epoch,list_id) VALUES(?,?)`, v.Epoch, list)
		return err
	})
}
func (s *Store) ApplyPage(ctx context.Context, v Snapshot, list string, items []map[string]any, cursor string, done bool, now time.Time) error {
	return s.transaction(ctx, v.Epoch, v.Revision, func(tx *sql.Tx) error {
		for _, m := range items {
			if _, removed := m["@removed"]; removed {
				if _, err := tx.Exec(`DELETE FROM tasks WHERE epoch=? AND list_id=? AND id=?`, v.Epoch, list, m["id"]); err != nil {
					return err
				}
				continue
			}
			b, _ := json.Marshal(m)
			if _, err := tx.Exec(`INSERT INTO tasks VALUES(?,?,?,?) ON CONFLICT(epoch,list_id,id) DO UPDATE SET payload=json_patch(tasks.payload,excluded.payload)`, v.Epoch, list, m["id"], string(b)); err != nil {
				return err
			}
		}
		ms := int64(0)
		if done {
			ms = now.UnixMilli()
		}
		_, err := tx.Exec(`INSERT INTO sync_state VALUES(?,?,?,?,?, '') ON CONFLICT(epoch,list_id) DO UPDATE SET cursor=excluded.cursor,complete=excluded.complete,last_success_at=excluded.last_success_at,last_error=''`, v.Epoch, list, cursor, done, ms)
		return err
	})
}
func (s *Store) SyncError(ctx context.Context, epoch int64, list, code string) {
	s.DB.ExecContext(ctx, `UPDATE sync_state SET last_error=? WHERE epoch=? AND list_id=?`, code, epoch, list)
}

// Query only runs after every requested scope has a complete, fresh baseline.
func (s *Store) Query(ctx context.Context, epoch int64, where string, args []any, limit, offset int) ([]map[string]any, int, error) {
	base := ` FROM tasks WHERE epoch=? ` + where
	params := append([]any{epoch}, args...)
	var total int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*)`+base, params...).Scan(&total); err != nil {
		return nil, 0, err
	}
	params = append(params, limit, offset)
	rows, err := s.rows(ctx, `SELECT json_set(payload,'$.listId',list_id)`+base+` ORDER BY coalesce(json_extract(payload,'$.lastModifiedDateTime'),'') DESC,list_id,id LIMIT ? OFFSET ?`, params...)
	return rows, total, err
}
func (s *Store) ConfirmTask(ctx context.Context, epoch int64, list, id string, data map[string]any, deleted bool) error {
	return s.transaction(ctx, epoch, -1, func(tx *sql.Tx) error {
		if deleted {
			_, err := tx.Exec(`DELETE FROM tasks WHERE epoch=? AND list_id=? AND id=?`, epoch, list, id)
			return err
		}
		if data["id"] == nil {
			return nil
		}
		b, _ := json.Marshal(data)
		_, err := tx.Exec(`INSERT INTO tasks VALUES(?,?,?,?) ON CONFLICT(epoch,list_id,id) DO UPDATE SET payload=json_patch(tasks.payload,excluded.payload)`, epoch, list, data["id"], string(b))
		return err
	})
}
func (s *Store) Status(ctx context.Context, epoch int64) ([]map[string]any, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT list_id,complete,last_success_at,last_error FROM sync_state WHERE epoch=? ORDER BY list_id`, epoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, e string
		var complete bool
		var ms int64
		if err = rows.Scan(&id, &complete, &ms, &e); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"listId": id, "complete": complete, "lastSuccessAt": ms, "lastError": e})
	}
	return out, rows.Err()
}
