package todo_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qaq112233/mstodo-go/internal/microsoft"
	"github.com/qaq112233/mstodo-go/internal/store"
	"github.com/qaq112233/mstodo-go/internal/testutil"
	"github.com/qaq112233/mstodo-go/internal/todo"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func myday(t *testing.T, f *testutil.Fixture, q url.Values) todo.Result {
	t.Helper()
	r, e := f.Todo.Call(context.Background(), todo.Operation{Kind: "myday", Query: q})
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func tasks(t *testing.T, res todo.Result) []map[string]any {
	t.Helper()
	b, _ := json.Marshal(res.Data)
	var v struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if e := json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	return v.Tasks
}
func TestMyDayFeedCacheAndFilters(t *testing.T) {
	var feed atomic.Int32
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "graph.microsoft.com" {
			writeJSON(w, map[string]any{"value": []any{map[string]any{"id": "L", "displayName": "工作"}}})
			return
		}
		feed.Add(1)
		if r.URL.Query().Get("date") != "2026-10-24" {
			t.Error("missing explicit local date", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer exo-token" || r.Header.Get("x-anchormailbox") != "OID:owner@tenant" {
			t.Error("wrong EXO headers")
		}
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, map[string]any{"value": []any{map[string]any{"Id": "done", "ParentFolderId": "unknown", "Subject": "完成", "Status": "Completed", "CommittedOrder": "2026-10-24T01:00:00Z"}}})
			return
		}
		writeJSON(w, map[string]any{"Value": []any{
			map[string]any{"Id": "open", "ParentFolderId": "L", "Status": "NotStarted", "CommittedDay": nil, "Categories": []string{"中文"}, "DueDateTime": map[string]any{"DateTime": "2026-10-24T00:00:00Z"}},
			map[string]any{"Id": "done", "ParentFolderId": "unknown", "Status": "NotStarted"},
		}, "NextLink": "/todob2/api/v1/myDayFeed/sections/tasks?date=2026-10-24&page=2"})
	})
	first := myday(t, f, nil)
	rows := tasks(t, first)
	if len(rows) != 2 || rows[0]["id"] != "done" || rows[0]["status"] != "Completed" || rows[0]["listName"] != nil || rows[1]["listName"] != "工作" || rows[1]["committedDay"] != nil || rows[1]["dueDate"] != "2026-10-24" {
		t.Fatalf("projection or membership: %#v", rows)
	}
	filtered := myday(t, f, url.Values{"includeCompleted": {"false"}})
	if len(tasks(t, filtered)) != 1 || !filtered.Hit || feed.Load() != 2 {
		t.Fatalf("filters must share cache: %d", feed.Load())
	}
	f.Clock.Add(179 * time.Second)
	if !myday(t, f, nil).Hit {
		t.Fatal("cache expired early")
	}
	f.Clock.Add(time.Second)
	if myday(t, f, nil).Hit || feed.Load() != 4 {
		t.Fatal("180 second boundary")
	}
}
func TestMyDayEmptyCacheAndFailureDoesNotRenew(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "graph.microsoft.com" {
			writeJSON(w, map[string]any{"value": []any{}})
			return
		}
		calls.Add(1)
		if fail.Load() {
			http.Error(w, "failure", 502)
			return
		}
		writeJSON(w, map[string]any{"Value": []any{}})
	})
	if len(tasks(t, myday(t, f, nil))) != 0 || !myday(t, f, nil).Hit || calls.Load() != 1 {
		t.Fatal("empty cache not reused")
	}
	before, _ := f.Store.GetCache(context.Background(), f.Epoch, "myday:Asia/Shanghai:2026-10-24")
	fail.Store(true)
	f.Clock.Add(180 * time.Second)
	for i := 0; i < 2; i++ {
		_, err := f.Todo.Call(context.Background(), todo.Operation{Kind: "myday"})
		if microsoft.AsError(err).Code != "substrate_error" {
			t.Fatal(err)
		}
	}
	after, _ := f.Store.GetCache(context.Background(), f.Epoch, "myday:Asia/Shanghai:2026-10-24")
	if !before.Fetched.Equal(after.Fetched) || calls.Load() != 5 {
		t.Fatal("failure cached or renewed", calls.Load())
	}
}
func TestMyDayConcurrentRefreshCoalesces(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "graph.microsoft.com" {
			writeJSON(w, map[string]any{"value": []any{}})
			return
		}
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		writeJSON(w, map[string]any{"Value": []any{}})
	})
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := f.Todo.Call(context.Background(), todo.Operation{Kind: "myday"})
			errs <- e
		}()
	}
	<-entered
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate refresh", calls.Load())
	}
}
func TestMyDayIncompleteNeverCached(t *testing.T) {
	cases := []struct {
		name, body, code string
		cap              int
	}{
		{"missing collection", `{}`, "substrate_error", 30},
		{"null collection", `{"Value":null}`, "substrate_error", 30},
		{"missing identity", `{"Value":[{"Id":"a"}]}`, "substrate_error", 30},
		{"bad identity", `{"Value":[{"Id":42,"ParentFolderId":"L"}]}`, "substrate_error", 30},
		{"bad categories", `{"Value":[{"Id":"a","ParentFolderId":"L","Categories":"bad"}]}`, "substrate_error", 30},
		{"bad link", `{"Value":[],"NextLink":42}`, "substrate_error", 30},
		{"foreign link", `{"Value":[],"NextLink":"https://evil.example/page"}`, "upstream_url_rejected", 30},
		{"ambiguous link", `{"Value":[],"NextLink":"//evil.example/page"}`, "upstream_url_rejected", 30},
		{"page cap", `{"Value":[],"NextLink":"/next"}`, "my_day_truncated", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, tc.body) })
			v, _ := f.Store.Snapshot(context.Background())
			cfg := v.Settings
			cfg.MyDayMaxPages = tc.cap
			if e := f.Store.SaveSettings(context.Background(), v, cfg); e != nil {
				t.Fatal(e)
			}
			_, err := f.Todo.Call(context.Background(), todo.Operation{Kind: "myday"})
			if microsoft.AsError(err).Code != tc.code {
				t.Fatalf("%v", err)
			}
			if calls.Load() != 1 {
				t.Fatal("unexpected requests", calls.Load())
			}
			if _, err = f.Store.GetCache(context.Background(), f.Epoch, "myday:Asia/Shanghai:2026-10-24"); err == nil {
				t.Fatal("partial cache saved")
			}
		})
	}
}

func TestLocalMidnightUsesDifferentCacheKey(t *testing.T) {
	var dates []string
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "graph.microsoft.com" {
			writeJSON(w, map[string]any{"value": []any{}})
			return
		}
		dates = append(dates, r.URL.Query().Get("date"))
		writeJSON(w, map[string]any{"Value": []any{}})
	})
	f.Clock.Add(-time.Millisecond)
	_ = myday(t, f, nil)
	f.Clock.Add(time.Millisecond)
	_ = myday(t, f, nil)
	if len(dates) != 2 || dates[0] != "2026-10-23" || dates[1] != "2026-10-24" {
		t.Fatal("midnight reused old day", dates)
	}
}

func TestAlreadyExpiredMyDayDeadlineHasStableError(t *testing.T) {
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) { t.Error("request after expired deadline") })
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := f.Todo.Call(ctx, todo.Operation{Kind: "myday"})
	if microsoft.AsError(err).Code != "my_day_timeout" {
		t.Fatal(err)
	}
}
func TestMyDayMetadataDeadlineAndNoRosterMutation(t *testing.T) {
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "graph.microsoft.com" {
			<-r.Context().Done()
			return
		}
		writeJSON(w, map[string]any{"Value": []any{map[string]any{"Id": "a", "ParentFolderId": "L"}}})
	})
	v, _ := f.Store.Snapshot(context.Background())
	if err := f.Store.SaveLists(context.Background(), v, []map[string]any{{"id": "L", "displayName": "缓存名称"}}, f.Clock.Now().Add(-181*time.Second)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	rows := tasks(t, myday(t, f, nil))
	elapsed := time.Since(start)
	if elapsed > 3*time.Second || rows[0]["listName"] != "缓存名称" {
		t.Fatal("metadata did not degrade", elapsed, rows)
	}
	lists, _ := f.Store.Lists(context.Background(), f.Epoch)
	if len(lists) != 1 {
		t.Fatal("roster overwritten")
	}
}
func TestMyDayOldReadCannotOverwriteSuccessfulWrite(t *testing.T) {
	var feeds atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "graph.microsoft.com" {
			if r.Method == "PATCH" {
				writeJSON(w, map[string]any{"id": "a", "title": "new"})
			} else {
				writeJSON(w, map[string]any{"value": []any{}})
			}
			return
		}
		n := feeds.Add(1)
		title := "new"
		if n == 1 {
			close(entered)
			<-release
			title = "old"
		}
		writeJSON(w, map[string]any{"Value": []any{map[string]any{"Id": "a", "ParentFolderId": "L", "Subject": title}}})
	})
	result := make(chan todo.Result, 1)
	errs := make(chan error, 1)
	go func() {
		r, e := f.Todo.Call(context.Background(), todo.Operation{Kind: "myday"})
		result <- r
		errs <- e
	}()
	<-entered
	if _, e := f.Todo.Call(context.Background(), todo.Operation{Kind: "update_task", ListID: "L", TaskID: "a", Args: map[string]any{"title": "new"}}); e != nil {
		t.Fatal(e)
	}
	close(release)
	res := <-result
	if e := <-errs; e != nil {
		t.Fatal(e)
	}
	if rows := tasks(t, res); rows[0]["title"] != "new" || feeds.Load() != 2 {
		t.Fatal("old read escaped fence", rows, feeds.Load())
	}
	if rows := tasks(t, myday(t, f, nil)); rows[0]["title"] != "new" {
		t.Fatal("old cache persisted")
	}
}
func TestMyDayBodyDeadline(t *testing.T) {
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err := f.Todo.Call(ctx, todo.Operation{Kind: "myday"})
	if microsoft.AsError(err).Code != "my_day_timeout" {
		t.Fatal(err)
	}
}
func TestMyDayInvalidParametersDoNotRequestMicrosoft(t *testing.T) {
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected upstream request") })
	for _, q := range []url.Values{{"date": {"2026-02-30"}}, {"date": {""}}, {"includeCompleted": {"False"}}, {"refresh": {"1"}}} {
		if _, e := f.Todo.Call(context.Background(), todo.Operation{Kind: "myday", Query: q}); e == nil {
			t.Fatal("accepted bad query", q)
		}
	}
}
func TestAccountSwitchFencesCachedWork(t *testing.T) {
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{"Value": []any{}}) })
	v, _ := f.Store.Snapshot(context.Background())
	if e := f.Store.InstallAccount(context.Background(), v.Epoch, store.Account{ID: "other"}); e != nil {
		t.Fatal(e)
	}
	if e := f.Store.PutCache(context.Background(), v, "old", map[string]any{}, f.Clock.Now()); e != store.ErrChanged {
		t.Fatal("old epoch accepted", e)
	}
}
