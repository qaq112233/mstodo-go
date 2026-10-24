package todo_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qaq112233/mstodo-go/internal/microsoft"
	"github.com/qaq112233/mstodo-go/internal/testutil"
	"github.com/qaq112233/mstodo-go/internal/todo"
)

func query(t *testing.T, f *testutil.Fixture, q url.Values) map[string]any {
	t.Helper()
	r, e := f.Todo.Call(context.Background(), todo.Operation{Kind: "query", Query: q})
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(r.Data)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}
func TestDemandDeltaChineseLiteralSearchAndDeletion(t *testing.T) {
	var delta, roster atomic.Int32
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.0/me/todo/lists" {
			roster.Add(1)
			writeJSON(w, map[string]any{"value": []any{map[string]any{"id": "L", "wellknownListName": "flaggedEmails"}}})
			return
		}
		delta.Add(1)
		if r.URL.Query().Get("delta") == "1" {
			writeJSON(w, map[string]any{"value": []any{map[string]any{"id": "b", "@removed": map[string]any{"reason": "deleted"}}, map[string]any{"id": "a", "title": "已更新中文"}}, "@odata.deltaLink": "https://graph.microsoft.com/v1.0/me/todo/lists/L/tasks/delta?delta=2"})
			return
		}
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, map[string]any{"value": []any{map[string]any{"id": "b", "title": "普通任务", "body": map[string]any{"content": "正文中文"}, "status": "completed", "importance": "normal"}}, "@odata.deltaLink": "https://graph.microsoft.com/v1.0/me/todo/lists/L/tasks/delta?delta=1"})
			return
		}
		writeJSON(w, map[string]any{"value": []any{map[string]any{"id": "a", "title": "中文_100%", "status": "notStarted", "importance": "high", "lastModifiedDateTime": "2026-10-24T01:00:00Z", "dueDateTime": map[string]any{"dateTime": "2026-10-25T00:00:00"}}}, "@odata.nextLink": "https://graph.microsoft.com/v1.0/me/todo/lists/L/tasks/delta?page=2"})
	})
	if delta.Load() != 0 || roster.Load() != 0 {
		t.Fatal("startup prewarmed task data")
	}
	all := query(t, f, nil)
	if all["total"] != float64(2) {
		t.Fatal(all)
	}
	if result := query(t, f, url.Values{"q": {"中文"}}); result["total"] != float64(2) {
		t.Fatal("Chinese substring", result)
	}
	if result := query(t, f, url.Values{"q": {"_100%"}, "importance": {"high"}, "status": {"notStarted"}, "dueFrom": {"2026-10-25"}, "dueTo": {"2026-10-25"}}); result["total"] != float64(1) {
		t.Fatal("literal LIKE", result)
	}
	if result := query(t, f, url.Values{"q": {"' OR 1=1 --"}}); result["total"] != float64(0) {
		t.Fatal("SQL injection", result)
	}
	if delta.Load() != 2 || roster.Load() != 1 {
		t.Fatal("warm queries re-fetched", delta.Load(), roster.Load())
	}
	f.Clock.Add(180 * time.Second)
	all = query(t, f, nil)
	if all["total"] != float64(1) || delta.Load() != 3 {
		t.Fatal("delta deletion/update not applied", all, delta.Load())
	}
	if result := query(t, f, url.Values{"q": {"已更新"}}); result["total"] != float64(1) {
		t.Fatal(result)
	}
}
func TestInterruptedDeltaPersistsPageAndResumes(t *testing.T) {
	var first, page2 atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			page2.Add(1)
			if fail.Load() {
				w.WriteHeader(503)
				return
			}
			writeJSON(w, map[string]any{"value": []any{map[string]any{"id": "b", "title": "B"}}, "@odata.deltaLink": "https://graph.microsoft.com/v1.0/me/todo/lists/L/tasks/delta?delta=1"})
			return
		}
		first.Add(1)
		writeJSON(w, map[string]any{"value": []any{map[string]any{"id": "a", "title": "A"}}, "@odata.nextLink": "https://graph.microsoft.com/v1.0/me/todo/lists/L/tasks/delta?page=2"})
	})
	_, e := f.Todo.Call(context.Background(), todo.Operation{Kind: "query", Query: url.Values{"listId": {"L"}}})
	if !microsoft.AsError(e).SyncIncomplete {
		t.Fatal("partial query succeeded", e)
	}
	state, _ := f.Store.Sync(context.Background(), f.Epoch, "L")
	if state.Complete || !strings.Contains(state.Cursor, "page=2") || !state.Success.IsZero() {
		t.Fatal("page progress lost", state)
	}
	fail.Store(false)
	all := query(t, f, url.Values{"listId": {"L"}})
	if all["total"] != float64(2) || first.Load() != 1 || page2.Load() != 3 {
		t.Fatal("did not resume", all, first.Load(), page2.Load())
	}
}
func TestExpiredDeltaCursorRebuildsBaseline(t *testing.T) {
	var initial atomic.Int32
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("delta") == "old" {
			w.WriteHeader(410)
			return
		}
		id := "a"
		if initial.Add(1) > 1 {
			id = "b"
		}
		writeJSON(w, map[string]any{"value": []any{map[string]any{"id": id, "title": id}}, "@odata.deltaLink": "https://graph.microsoft.com/v1.0/me/todo/lists/L/tasks/delta?delta=old"})
	})
	_ = query(t, f, url.Values{"listId": {"L"}})
	f.Clock.Add(180 * time.Second)
	result := query(t, f, url.Values{"listId": {"L"}})
	rows := result["tasks"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["id"] != "b" || initial.Load() != 2 {
		t.Fatal("old baseline leaked", result)
	}
}
func TestRequiredListFailureCannotSilentlyNarrowSearch(t *testing.T) {
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/bad/") {
			w.WriteHeader(403)
			return
		}
		writeJSON(w, map[string]any{"value": []any{map[string]any{"id": "a", "title": "A"}}, "@odata.deltaLink": "https://graph.microsoft.com/v1.0/me/todo/lists/ok/tasks/delta?end=1"})
	})
	_, e := f.Todo.Call(context.Background(), todo.Operation{Kind: "query", Query: url.Values{"listId": {"ok", "bad"}}})
	if e == nil || !microsoft.AsError(e).SyncIncomplete {
		t.Fatal("partial scope returned as success")
	}
}
func TestPatchNullAndWriteDoesNotRefreshWholeList(t *testing.T) {
	var patch map[string]any
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			_ = json.NewDecoder(r.Body).Decode(&patch)
			writeJSON(w, map[string]any{"id": "a", "title": "changed", "status": "completed"})
			return
		}
		writeJSON(w, map[string]any{"value": []any{map[string]any{"id": "a", "title": "A"}}, "@odata.deltaLink": "https://graph.microsoft.com/v1.0/me/todo/lists/L/tasks/delta?d=1"})
	})
	_ = query(t, f, url.Values{"listId": {"L"}})
	op := todo.Operation{Kind: "update_task", ListID: "L", TaskID: "a", Args: map[string]any{"status": "completed", "dueDateTime": nil, "reminderDateTime": nil, "recurrence": nil}}
	if _, e := f.Todo.Call(context.Background(), op); e != nil {
		t.Fatal(e)
	}
	if len(patch) != 4 || patch["dueDateTime"] != nil || patch["recurrence"] != nil {
		t.Fatal("null semantics lost", patch)
	}
	if _, present := patch["title"]; present {
		t.Fatal("omitted field overwritten")
	}
	state, _ := f.Store.Sync(context.Background(), f.Epoch, "L")
	if !state.Success.IsZero() {
		t.Fatal("task write made whole list fresh")
	}
}
func TestMoveUsesReparentOnlyAndReturnsNewIdentity(t *testing.T) {
	for _, confirm := range []bool{true, false} {
		t.Run(fmt.Sprint(confirm), func(t *testing.T) {
			var calls atomic.Int32
			f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Host != "substrate.office.com" || r.Method != "PATCH" || r.URL.Path != "/todob2/api/v1/taskfolders/dest/tasks/old" {
					t.Error("unexpected move route", r.Method, r.URL)
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if len(body) != 1 || body["ParentFolderId"] != "dest" {
					t.Error("move must preserve same underlying task", body)
				}
				if confirm {
					writeJSON(w, map[string]any{"Id": "new", "ParentFolderId": "dest", "HasAttachments": true})
				} else {
					writeJSON(w, map[string]any{"ParentFolderId": "source"})
				}
			})
			r, e := f.Todo.Call(context.Background(), todo.Operation{Kind: "move", ListID: "source", TaskID: "old", Args: map[string]any{"toListId": "dest"}})
			if confirm {
				if e != nil || r.Data.(map[string]any)["taskId"] != "new" || r.Data.(map[string]any)["previousTaskId"] != "old" {
					t.Fatal(r, e)
				}
			} else {
				if microsoft.AsError(e).Code != "move_outcome_unknown" || !microsoft.AsError(e).Unknown {
					t.Fatal(e)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("copy/delete or retry path invoked")
			}
		})
	}
}
func TestMyDayWritesAndThreeOrderScopes(t *testing.T) {
	var field, path string
	var body map[string]any
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			path = r.URL.Path
			body = nil
			_ = json.NewDecoder(r.Body).Decode(&body)
			for k := range body {
				field = k
			}
			writeJSON(w, map[string]any{"Id": "a", "ParentFolderId": "L"})
			return
		}
		if r.Host == "graph.microsoft.com" {
			writeJSON(w, map[string]any{"value": []any{}})
			return
		}
		if strings.Contains(r.URL.Path, "myDayFeed") {
			writeJSON(w, map[string]any{"Value": []any{
				map[string]any{"Id": "a", "ParentFolderId": "L", "Status": "NotStarted"},
				map[string]any{"Id": "b", "ParentFolderId": "OTHER", "Status": "NotStarted", "CommittedOrder": "2026-10-24T02:00:00Z"},
				map[string]any{"Id": "done", "ParentFolderId": "OTHER", "Status": "Completed", "CommittedOrder": "2099-10-24T02:00:00Z"},
			}})
			return
		}
		if r.URL.Query().Get("$select") == "*" {
			writeJSON(w, map[string]any{"Id": "a", "Subtasks": []any{map[string]any{"Id": "x"}, map[string]any{"Id": "y", "OrderDateTime": "2026-10-24T03:00:00Z"}}})
			return
		}
		writeJSON(w, map[string]any{"Value": []any{map[string]any{"Id": "a"}, map[string]any{"Id": "b", "OrderDateTime": "2026-10-24T01:00:00Z"}}})
	})
	for _, tc := range []struct{ kind, item, wantField, wantPath, wantTime string }{
		{"reorder_task", "", "OrderDateTime", "/todob2/api/v1/taskfolders/L/tasks/a", "2026-10-24T01:01:00.000Z"},
		{"reorder_checklist", "x", "OrderDateTime", "/todob2/api/v1/tasks/a/subtasks/x", "2026-10-24T03:01:00.000Z"},
		{"reorder_myday", "", "CommittedOrder", "/todob2/api/v1/taskfolders/L/tasks/a", "2026-10-24T02:01:00.000Z"},
	} {
		args := map[string]any{"position": "top"}
		if tc.kind == "reorder_myday" {
			args["listId"] = "L"
			args["taskId"] = "a"
		}
		_, e := f.Todo.Call(context.Background(), todo.Operation{Kind: tc.kind, ListID: "L", TaskID: "a", ItemID: tc.item, Args: args})
		if e != nil {
			t.Fatal(tc.kind, e)
		}
		if field != tc.wantField || path != tc.wantPath || body[tc.wantField] != tc.wantTime {
			t.Fatal("scope mixed", tc.kind, field, path, body)
		}
	}
	_, e := f.Todo.Call(context.Background(), todo.Operation{Kind: "add_myday", ListID: "L", TaskID: "a"})
	if e != nil {
		t.Fatal(e)
	}
	if body["CommittedDay"] != "2026-10-24" || body["PostponedDay"] != nil || body["CommittedOrder"] == nil {
		t.Fatal(body)
	}
	_, e = f.Todo.Call(context.Background(), todo.Operation{Kind: "remove_myday", ListID: "L", TaskID: "a"})
	if e != nil || len(body) != 1 || body["CommittedDay"] != nil {
		t.Fatal(body, e)
	}
}
func TestOrderArithmeticMatchesBaseline(t *testing.T) {
	for _, tc := range []struct {
		pos       string
		index     int
		ref       int64
		orders    []int64
		want      int64
		precision bool
	}{
		{pos: "top", orders: []int64{100, 300}, want: 60300},
		{pos: "bottom", orders: []int64{100, 300}, want: -59900},
		{pos: "index", index: 2, orders: []int64{100, 300}, want: 200},
		{pos: "before", ref: 100, orders: []int64{60100, 100}, want: 30100},
		{pos: "after", ref: 300, orders: []int64{300, 100}, want: 200},
		{pos: "index", index: 2, orders: []int64{300, 299}, precision: true},
		{pos: "top", want: 1234}, {pos: "bottom", want: 1234},
	} {
		got, e := todo.ComputeOrder(tc.pos, tc.index, tc.orders, tc.ref, 1234)
		if tc.precision {
			if microsoft.AsError(e).Code != "order_precision_exhausted" {
				t.Fatal(e)
			}
		} else if e != nil || got != tc.want {
			t.Fatal(tc, got, e)
		}
	}
}

func TestCancelledQueryStopsWithoutBackgroundContinuation(t *testing.T) {
	page2, cancelled := make(chan struct{}), make(chan struct{})
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			close(page2)
			<-r.Context().Done()
			close(cancelled)
			return
		}
		writeJSON(w, map[string]any{"value": []any{map[string]any{"id": "a"}}, "@odata.nextLink": "https://graph.microsoft.com/v1.0/me/todo/lists/L/tasks/delta?page=2"})
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.Todo.Call(ctx, todo.Operation{Kind: "query", Query: url.Values{"listId": {"L"}}})
		done <- err
	}()
	<-page2
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled query succeeded")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("query continued without a caller")
	}
	f.Todo.Wait()
	state, _ := f.Store.Sync(context.Background(), f.Epoch, "L")
	if state.Complete || !strings.Contains(state.Cursor, "page=2") {
		t.Fatal("cancel lost durable page progress", state)
	}
}

func TestManualReadPagesAndCachesWholeCollection(t *testing.T) {
	var calls atomic.Int32
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Host != "substrate.office.com" {
			t.Error("manual query used Graph")
		}
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, map[string]any{"Value": []any{map[string]any{"Id": "second", "Status": "Completed"}, map[string]any{"Id": "third", "Status": "NotStarted"}}})
			return
		}
		writeJSON(w, map[string]any{"Value": []any{map[string]any{"Id": "first", "Status": "NotStarted", "OrderDateTime": "2026-10-24T01:00:00Z", "DueDateTime": map[string]any{"DateTime": "2026-10-24T00:00:00", "TimeZone": "China Standard Time"}}}, "NextLink": "/todob2/api/v1/taskfolders/L/tasks?page=2"})
	})
	op := todo.Operation{Kind: "list_tasks", ListID: "L", Query: url.Values{"order": {"manual"}, "limit": {"1"}}}
	r, e := f.Todo.Call(context.Background(), op)
	if e != nil {
		t.Fatal(e)
	}
	m := r.Data.(map[string]any)
	rows := m["tasks"].([]map[string]any)
	if rows[0]["id"] != "first" || rows[0]["dueDateTime"].(map[string]any)["timeZone"] != "China Standard Time" || m["total"] != 3 {
		t.Fatal(m)
	}
	op.Query.Set("offset", "1")
	r, e = f.Todo.Call(context.Background(), op)
	if e != nil {
		t.Fatal(e)
	}
	if r.Data.(map[string]any)["tasks"].([]map[string]any)[0]["id"] != "second" || calls.Load() != 2 || !r.Hit {
		t.Fatal("cache/pagination lost upstream order", r, calls.Load())
	}
}

func TestChecklistReadJoinsInlineManualOrder(t *testing.T) {
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "graph.microsoft.com" {
			writeJSON(w, map[string]any{"value": []any{map[string]any{"id": "a", "createdDateTime": "2026-10-23T00:00:00Z"}, map[string]any{"id": "b", "createdDateTime": "2026-10-24T00:00:00Z"}}})
			return
		}
		if r.URL.Path != "/todob2/api/v1/taskfolders/L/tasks/T" || r.URL.Query().Get("$select") != "*" {
			t.Error("steps were not read inline")
		}
		writeJSON(w, map[string]any{"Id": "T", "Subtasks": []any{map[string]any{"Id": "a", "OrderDateTime": nil}, map[string]any{"Id": "b", "OrderDateTime": "2026-10-24T01:00:00Z"}}})
	})
	r, e := f.Todo.Call(context.Background(), todo.Operation{Kind: "list_checklist", ListID: "L", TaskID: "T"})
	if e != nil {
		t.Fatal(e)
	}
	items := r.Data.([]map[string]any)
	if len(items) != 2 || items[0]["id"] != "b" || items[1]["id"] != "a" {
		t.Fatal(items)
	}
}

func TestOlderDeltaReadRetriesAfterConcurrentWrite(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var pages atomic.Int32
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			writeJSON(w, map[string]any{"id": "a", "title": "new"})
			return
		}
		title := "new"
		if pages.Add(1) == 1 {
			close(entered)
			<-release
			title = "old"
		}
		writeJSON(w, map[string]any{"value": []any{map[string]any{"id": "a", "title": title}}, "@odata.deltaLink": "https://graph.microsoft.com/v1.0/me/todo/lists/L/tasks/delta?end=1"})
	})
	done := make(chan todo.Result, 1)
	errs := make(chan error, 1)
	go func() {
		r, e := f.Todo.Call(context.Background(), todo.Operation{Kind: "query", Query: url.Values{"listId": {"L"}}})
		done <- r
		errs <- e
	}()
	<-entered
	if _, e := f.Todo.Call(context.Background(), todo.Operation{Kind: "update_task", ListID: "L", TaskID: "a", Args: map[string]any{"title": "new"}}); e != nil {
		t.Fatal(e)
	}
	close(release)
	res := <-done
	if e := <-errs; e != nil {
		t.Fatal(e)
	}
	rows := res.Data.(map[string]any)["tasks"].([]map[string]any)
	if rows[0]["title"] != "new" || pages.Load() != 2 {
		t.Fatal("older delta escaped revision fence", rows, pages.Load())
	}
}
