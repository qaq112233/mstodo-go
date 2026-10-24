// Package todo is the shared business layer used directly by REST and MCP.
package todo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qaq112233/mstodo-go/internal/microsoft"
	"github.com/qaq112233/mstodo-go/internal/store"
)

type Object = map[string]any
type Operation struct {
	Kind, ListID, TaskID, ItemID string
	Args                         Object
	Query                        url.Values
	IfMatch                      string
}
type Result struct {
	Data    any
	Status  int
	Fetched time.Time
	Hit     bool
	Warning string
}
type Service struct {
	Store   *store.Store
	MS      *microsoft.Client
	Now     func() time.Time
	mu      sync.Mutex
	flights map[string]*readFlight
	writes  chan struct{}
	dirty   atomic.Bool
	work    sync.WaitGroup
}

func (s *Service) Wait() { s.work.Wait() }

type readFlight struct {
	done   chan struct{}
	cancel context.CancelFunc
	users  int
	value  any
	err    error
}

// A shared data read is cancelled when its last caller leaves. Token refresh
// deliberately has a different lifetime, owned by microsoft.Manager.
func (s *Service) flight(ctx context.Context, key string, fn func(context.Context) (any, error)) (any, error) {
	s.mu.Lock()
	f := s.flights[key]
	if f == nil {
		shared, cancel := context.WithCancel(context.Background())
		f = &readFlight{done: make(chan struct{}), cancel: cancel}
		s.flights[key] = f
		s.work.Add(1)
		go func() {
			defer s.work.Done()
			defer cancel()
			f.value, f.err = fn(shared)
			s.mu.Lock()
			if s.flights[key] == f {
				delete(s.flights, key)
			}
			close(f.done)
			s.mu.Unlock()
		}()
	}
	f.users++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		f.users--
		if f.users == 0 {
			f.cancel()
			if s.flights[key] == f {
				delete(s.flights, key)
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		return f.value, f.err
	}
}

func New(s *store.Store, m *microsoft.Client) *Service {
	return &Service{Store: s, MS: m, Now: time.Now, writes: make(chan struct{}, 1), flights: map[string]*readFlight{}}
}
func str(v any) string { s, _ := v.(string); return s }
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func day(v any) any {
	s := str(v)
	if len(s) >= 10 {
		return s[:10]
	}
	return nil
}
func dateTime(v any) any {
	if m, ok := v.(map[string]any); ok {
		return nullable(str(m["DateTime"]))
	}
	return nullable(str(v))
}
func ValidDate(s string) bool {
	t, e := time.Parse("2006-01-02", s)
	return e == nil && t.Format("2006-01-02") == s
}
func Bool(q url.Values, key string, def bool) (bool, error) {
	values, present := q[key]
	if !present {
		return def, nil
	}
	if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
		return false, microsoft.E(400, "invalid_"+camelSnake(key))
	}
	return values[0] == "true", nil
}
func camelSnake(s string) string {
	if s == "includeCompleted" {
		return "include_completed"
	}
	return s
}
func (s *Service) date(v store.Snapshot, q url.Values) (string, error) {
	date := q.Get("date")
	if _, ok := q["date"]; !ok {
		loc, err := time.LoadLocation(v.Settings.Timezone)
		if err != nil {
			return "", err
		}
		date = s.Now().In(loc).Format("2006-01-02")
	}
	if !ValidDate(date) {
		return "", microsoft.E(400, "invalid_date")
	}
	return date, nil
}
func (s *Service) cached(ctx context.Context, v store.Snapshot, key string, refresh bool, budget time.Duration, load func(context.Context, store.Snapshot) (any, error)) (Result, error) {
	if !refresh {
		c, err := s.Store.GetCache(ctx, v.Epoch, key)
		if err == nil && s.Now().Before(c.Expires) {
			var data any
			if err = json.Unmarshal(c.Data, &data); err != nil {
				return Result{}, err
			}
			return Result{Data: data, Status: 200, Fetched: c.Fetched, Hit: true}, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Result{}, err
		}
	}
	flightKey := fmt.Sprintf("%d:%d:%s", v.Epoch, v.Revision, key)
	value, err := s.flight(ctx, flightKey, func(root context.Context) (any, error) {
		// Demand initiated, bounded work. It stops at the scope deadline and never
		// schedules another task sync; token refresh has its own persistence lifetime.
		// Each caller owns its deadline. Keep shared work alive for the other
		// waiters until the scope budget expires or the last caller leaves.
		shared, cancel := context.WithTimeout(root, budget)
		defer cancel()
		current, err := s.Store.Snapshot(shared)
		if err != nil {
			return nil, err
		}
		if current.Epoch != v.Epoch || current.Revision != v.Revision {
			return nil, store.ErrChanged
		}
		if !refresh {
			c, e := s.Store.GetCache(shared, v.Epoch, key)
			if e == nil && s.Now().Before(c.Expires) {
				var data any
				if e = json.Unmarshal(c.Data, &data); e != nil {
					return nil, e
				}
				return Result{Data: data, Status: 200, Fetched: c.Fetched, Hit: true}, nil
			}
		}
		data, err := load(shared, v)
		if err != nil {
			return nil, err
		}
		now := s.Now()
		if err = s.Store.PutCache(shared, v, key, data, now); err != nil {
			return nil, err
		}
		return Result{Data: data, Status: 200, Fetched: now}, nil
	})
	if err != nil {
		return Result{}, err
	}
	return value.(Result), nil
}
func (s *Service) Call(ctx context.Context, op Operation) (result Result, callErr error) {
	defer func() {
		if callErr != nil {
			e := microsoft.AsError(callErr)
			if e.Code == "upstream_timeout" && op.Kind == "myday" {
				e = microsoft.E(504, "my_day_timeout")
			}
			if e.Code == "upstream_timeout" && (op.Kind == "query" || op.Kind == "list_tasks") {
				e.SyncIncomplete = true
			}
			callErr = e
		}
	}()
	if op.Query == nil {
		op.Query = url.Values{}
	}
	if op.Args == nil {
		op.Args = Object{}
	}
	budget := 60 * time.Second
	if op.Kind == "myday" {
		budget = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var epoch int64
	for attempt := 0; attempt < 3; attempt++ {
		v, err := s.Store.Snapshot(ctx)
		if err != nil {
			return Result{}, err
		}
		if attempt == 0 {
			epoch = v.Epoch
		} else if v.Epoch != epoch {
			// A revision retry may refresh data, but must never rebind the
			// original operation (including writes) to a new authorization.
			return Result{}, microsoft.E(409, "account_changed")
		}
		if s.dirty.Load() {
			if err = s.Store.Invalidate(ctx, v.Epoch, nil, true); err != nil {
				return Result{}, err
			}
			s.dirty.Store(false)
			continue
		}
		res, err := s.call(ctx, v, op)
		if errors.Is(err, store.ErrChanged) {
			continue
		}
		if err != nil {
			return Result{}, err
		}
		// The remote write is confirmed. A cancelled caller or a subsequent
		// database problem must not turn it into an apparent failed creation.
		if !isRead(op.Kind) {
			return res, nil
		}
		// Recheck after reading so an account switch cannot deliver another account's
		// cached result or commit a stale snapshot under the new authorization.
		latest, e := s.Store.Snapshot(ctx)
		if e != nil {
			return Result{}, e
		}
		if latest.Epoch != v.Epoch {
			return Result{}, microsoft.E(409, "account_changed")
		}
		if latest.Revision != v.Revision && isRead(op.Kind) {
			continue
		}
		return res, nil
	}
	return Result{}, microsoft.E(409, "data_changed_retry")
}
func (s *Service) call(ctx context.Context, v store.Snapshot, op Operation) (Result, error) {
	refresh, err := Bool(op.Query, "refresh", false)
	if err != nil {
		return Result{}, err
	}
	switch op.Kind {
	case "status":
		a, e := s.Store.Account(ctx, v.Epoch)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return Result{}, e
		}
		sync, e := s.Store.Status(ctx, v.Epoch)
		if e != nil {
			return Result{}, e
		}
		return Result{Status: 200, Data: Object{"connected": a.ID != "", "account": nullable(a.Email), "displayName": nullable(a.Name), "reauthRequired": a.Reauth, "myDayConsentRequired": a.SubstrateConsent, "timezone": v.Settings.Timezone, "cacheTTL": 180, "sync": sync, "tokenMaintenanceAt": a.Maintained, "tokenMaintenanceError": a.MaintenanceError}}, nil
	case "myday":
		return s.myday(ctx, v, op.Query, refresh)
	case "lists":
		data, err := s.lists(ctx, v, refresh)
		return data, err
	case "query":
		return s.query(ctx, v, op, refresh)
	case "list_tasks":
		if op.Query.Get("order") == "manual" {
			return s.manual(ctx, v, op, refresh)
		}
		if op.Query.Get("order") != "" {
			return Result{}, microsoft.E(400, "invalid_order")
		}
		op.Query.Set("listId", op.ListID)
		return s.query(ctx, v, op, refresh)
	case "get_list", "get_task", "list_checklist", "get_checklist", "list_links", "get_link":
		raw := graphURL(op)
		return s.cached(ctx, v, "graph:"+raw, refresh, 60*time.Second, func(c context.Context, v store.Snapshot) (any, error) {
			if op.Kind == "list_links" {
				return s.graphCollection(c, v, raw)
			}
			if op.Kind == "list_checklist" {
				return s.checklist(c, v, op)
			}
			b, e := s.MS.Do(c, v.Epoch, "graph", "GET", raw, nil, "")
			if e != nil {
				return nil, e
			}
			var m Object
			if json.Unmarshal(b, &m) != nil || str(m["id"]) == "" {
				return nil, microsoft.E(502, "graph_error")
			}
			return m, nil
		})
	default:
		return s.mutate(ctx, v, op)
	}
}
func isRead(kind string) bool {
	return oneOf(kind, "status", "myday", "lists", "query", "list_tasks", "get_list", "get_task", "list_checklist", "get_checklist", "list_links", "get_link")
}
func (s *Service) graphCollection(ctx context.Context, v store.Snapshot, raw string) ([]Object, error) {
	result := []Object{}
	seen := map[string]bool{}
	for pages := 0; raw != ""; pages++ {
		if pages >= 1000 || seen[raw] {
			return nil, microsoft.E(502, "collection_incomplete")
		}
		seen[raw] = true
		b, err := s.MS.Do(ctx, v.Epoch, "graph", "GET", raw, nil, "")
		if err != nil {
			return nil, err
		}
		items, next, _, err := parsePage(b, false, false)
		if err != nil {
			return nil, err
		}
		result = append(result, items...)
		if next != "" {
			next, err = microsoft.Link(next, "graph.microsoft.com")
			if err != nil {
				return nil, err
			}
		}
		raw = next
	}
	return result, nil
}
func parsePage(b []byte, substrate, folder bool) ([]Object, string, string, error) {
	code := "graph_error"
	if substrate {
		code = "substrate_error"
	}
	var env Object
	var raw any
	if folder && len(b) > 0 && b[0] == '[' {
		if json.Unmarshal(b, &raw) != nil {
			return nil, "", "", microsoft.E(502, code)
		}
	} else {
		if json.Unmarshal(b, &env) != nil || env == nil {
			return nil, "", "", microsoft.E(502, code)
		}
		raw = env["value"]
		if substrate && env["Value"] != nil {
			raw = env["Value"]
		}
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, "", "", microsoft.E(502, code)
	}
	items := make([]Object, 0, len(arr))
	for _, val := range arr {
		m, ok := val.(map[string]any)
		key := "id"
		if substrate {
			key = "Id"
		}
		if !ok || strings.TrimSpace(str(m[key])) == "" {
			return nil, "", "", microsoft.E(502, code)
		}
		if substrate && !validSubstrate(m) {
			return nil, "", "", microsoft.E(502, code)
		}
		if !substrate && !validGraph(m) {
			return nil, "", "", microsoft.E(502, code)
		}
		if substrate && !folder && strings.TrimSpace(str(m["ParentFolderId"])) == "" {
			return nil, "", "", microsoft.E(502, code)
		}
		items = append(items, m)
	}
	var next, delta string
	for _, key := range []string{"@odata.nextLink", "@odata.NextLink", "nextLink", "NextLink"} {
		if val, ok := env[key]; ok && val != nil && val != "" {
			s, ok := val.(string)
			if !ok {
				return nil, "", "", microsoft.E(502, code)
			}
			if next != "" && next != s {
				return nil, "", "", microsoft.E(502, code)
			}
			next = s
		}
	}
	if val, ok := env["@odata.deltaLink"]; ok && val != nil {
		var valid bool
		delta, valid = val.(string)
		if !valid {
			return nil, "", "", microsoft.E(502, code)
		}
	}
	if next != "" && delta != "" {
		return nil, "", "", microsoft.E(502, code)
	}
	return items, next, delta, nil
}

func validSubstrate(m Object) bool {
	for _, key := range []string{"Subject", "ParentFolderId", "Status", "Importance", "CommittedDay", "PostponedDay", "CommittedOrder", "OrderDateTime", "CreatedDateTime", "LastModifiedDateTime"} {
		if val, ok := m[key]; ok && val != nil {
			if _, ok = val.(string); !ok {
				return false
			}
		}
	}
	for _, key := range []string{"IsReminderOn", "HasAttachments"} {
		if val, ok := m[key]; ok && val != nil {
			if _, ok = val.(bool); !ok {
				return false
			}
		}
	}
	if val, ok := m["Categories"]; ok && val != nil {
		a, ok := val.([]any)
		if !ok {
			return false
		}
		for _, v := range a {
			if _, ok := v.(string); !ok {
				return false
			}
		}
	}
	for _, key := range []string{"DueDateTime", "StartDateTime", "CompletedDateTime", "ReminderDateTime"} {
		if val, ok := m[key]; ok && val != nil {
			if _, ok := val.(string); ok {
				continue
			}
			dt, ok := val.(map[string]any)
			if !ok {
				return false
			}
			for _, k := range []string{"DateTime", "TimeZone"} {
				if v, ok := dt[k]; ok {
					if _, ok = v.(string); !ok {
						return false
					}
				}
			}
		}
	}
	return true
}
func validGraph(m Object) bool {
	if v, ok := m["@removed"]; ok {
		_, ok = v.(map[string]any)
		return ok
	}
	for _, key := range []string{"title", "displayName", "status", "importance", "lastModifiedDateTime"} {
		if v, ok := m[key]; ok && v != nil {
			if _, ok = v.(string); !ok {
				return false
			}
		}
	}
	return true
}
func (s *Service) lists(ctx context.Context, v store.Snapshot, refresh bool) (Result, error) {
	return s.cached(ctx, v, "lists", refresh, 60*time.Second, func(c context.Context, v store.Snapshot) (any, error) {
		items, err := s.graphCollection(c, v, microsoft.GraphBase+"/me/todo/lists")
		if err != nil {
			return nil, err
		}
		if err = s.Store.SaveLists(c, v, items, s.Now()); err != nil {
			return nil, err
		}
		return items, nil
	})
}
func (s *Service) substrateCollection(ctx context.Context, v store.Snapshot, raw string, maxPages int, truncated string, folder bool) ([]Object, error) {
	result := []Object{}
	ids := map[string]int{}
	seen := map[string]bool{}
	for p := 0; raw != ""; p++ {
		if p >= maxPages || seen[raw] {
			return nil, microsoft.E(502, truncated)
		}
		seen[raw] = true
		b, err := s.MS.Do(ctx, v.Epoch, "substrate", "GET", raw, nil, "")
		if err != nil {
			return nil, err
		}
		items, next, _, err := parsePage(b, true, folder)
		if err != nil {
			return nil, err
		}
		for _, m := range items {
			id := str(m["Id"])
			if i, ok := ids[id]; ok {
				result[i] = m
			} else {
				ids[id] = len(result)
				result = append(result, m)
			}
		}
		if next != "" {
			next, err = microsoft.Link(next, "substrate.office.com")
			if err != nil {
				return nil, err
			}
		}
		raw = next
	}
	return result, nil
}
func (s *Service) myday(ctx context.Context, v store.Snapshot, q url.Values, refresh bool) (Result, error) {
	date, err := s.date(v, q)
	if err != nil {
		return Result{}, err
	}
	include, err := Bool(q, "includeCompleted", true)
	if err != nil {
		return Result{}, err
	}
	key := "myday:" + v.Settings.Timezone + ":" + date
	res, err := s.cached(ctx, v, key, refresh, 15*time.Second, func(c context.Context, v store.Snapshot) (any, error) {
		items, err := s.substrateCollection(c, v, microsoft.SubstrateBase+"/myDayFeed/sections/tasks?date="+date, v.Settings.MyDayMaxPages, "my_day_truncated", false)
		if err != nil {
			return nil, err
		}
		// Roster enrichment is deliberately read-only. Never replace/delete the
		// shared roster with this optional two-second, potentially older snapshot.
		cached, err := s.Store.Lists(c, v.Epoch)
		if err != nil {
			return nil, err
		}
		names := map[string]string{}
		for _, m := range cached {
			names[str(m["id"])] = str(m["displayName"])
		}
		roster, e := s.Store.GetCache(c, v.Epoch, "lists")
		if e == nil && s.Now().Before(roster.Expires) {
			var ls []Object
			if json.Unmarshal(roster.Data, &ls) == nil {
				for _, m := range ls {
					names[str(m["id"])] = str(m["displayName"])
				}
			}
		} else {
			meta, cancel := context.WithTimeout(c, 2*time.Second)
			ls, e := s.graphCollection(meta, v, microsoft.GraphBase+"/me/todo/lists")
			cancel()
			if e == nil {
				for _, m := range ls {
					names[str(m["id"])] = str(m["displayName"])
				}
			}
		}
		tasks := make([]Object, 0, len(items))
		for _, t := range items {
			categories := t["Categories"]
			if categories == nil {
				categories = []any{}
			}
			tasks = append(tasks, Object{"id": t["Id"], "listId": t["ParentFolderId"], "listName": nullable(names[str(t["ParentFolderId"])]), "title": t["Subject"], "status": t["Status"], "importance": t["Importance"], "dueDate": day(dateTime(t["DueDateTime"])), "startDate": day(dateTime(t["StartDateTime"])), "completedDate": dateTime(t["CompletedDateTime"]), "isReminderOn": t["IsReminderOn"], "reminderDate": dateTime(t["ReminderDateTime"]), "categories": categories, "committedDay": day(t["CommittedDay"]), "committedOrder": t["CommittedOrder"], "orderDateTime": t["OrderDateTime"], "postponedDay": day(t["PostponedDay"]), "lastModifiedDate": t["LastModifiedDateTime"]})
		}
		sort.SliceStable(tasks, func(i, j int) bool { return compareTasks(tasks[i], tasks[j]) })
		return Object{"date": date, "timezone": v.Settings.Timezone, "count": len(tasks), "tasks": tasks}, nil
	})
	if err != nil {
		return Result{}, err
	}
	// Decode a private copy: different completion filters share one raw snapshot.
	b, _ := json.Marshal(res.Data)
	var data struct {
		Date     string   `json:"date"`
		Timezone string   `json:"timezone"`
		Count    int      `json:"count"`
		Tasks    []Object `json:"tasks"`
	}
	if err = json.Unmarshal(b, &data); err != nil {
		return Result{}, err
	}
	if !include {
		filtered := []Object{}
		for _, t := range data.Tasks {
			if !strings.EqualFold(str(t["status"]), "completed") {
				filtered = append(filtered, t)
			}
		}
		data.Tasks = filtered
		data.Count = len(filtered)
	}
	res.Data = data
	return res, nil
}
func compareTasks(a, b Object) bool {
	for _, key := range []string{"committedOrder", "orderDateTime"} {
		x, y := timeOrderKey(a[key]), timeOrderKey(b[key])
		if x != y {
			return x > y
		}
	}
	if a["listId"] != b["listId"] {
		return str(a["listId"]) < str(b["listId"])
	}
	return str(a["id"]) < str(b["id"])
}

// RFC3339 strings are only lexically ordered when their offsets and fractional
// precision agree. Normalize comparison keys without changing returned fields.
func timeOrderKey(v any) string {
	raw := str(v)
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	return raw
}

func graphURL(op Operation) string {
	raw := microsoft.GraphBase + "/me/todo/lists"
	if op.ListID != "" {
		raw += "/" + url.PathEscape(op.ListID)
	}
	if op.TaskID != "" {
		raw += "/tasks/" + url.PathEscape(op.TaskID)
	} else if op.Kind == "create_task" {
		raw += "/tasks"
	}
	if strings.Contains(op.Kind, "checklist") {
		raw += "/checklistItems"
	} else if op.Kind == "create_link" || op.Kind == "update_link" || op.Kind == "delete_link" || op.Kind == "get_link" || op.Kind == "list_links" {
		raw += "/linkedResources"
	}
	if op.ItemID != "" {
		raw += "/" + url.PathEscape(op.ItemID)
	}
	return raw
}
func (s *Service) checklist(ctx context.Context, v store.Snapshot, op Operation) (any, error) {
	items, err := s.graphCollection(ctx, v, graphURL(op))
	if err != nil {
		return nil, err
	}
	raw := microsoft.SubstrateBase + "/taskfolders/" + url.PathEscape(op.ListID) + "/tasks/" + url.PathEscape(op.TaskID) + "?$select=*"
	b, err := s.MS.Do(ctx, v.Epoch, "substrate", "GET", raw, nil, "")
	if err != nil {
		return nil, err
	}
	subs, err := subtasks(b)
	if err != nil {
		return nil, err
	}
	orders := map[string]any{}
	for _, m := range subs {
		orders[str(m["Id"])] = m["OrderDateTime"]
	}
	for _, m := range items {
		m["orderDateTime"] = orders[str(m["id"])]
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := timeOrderKey(items[i]["orderDateTime"]), timeOrderKey(items[j]["orderDateTime"])
		if a != b {
			return a > b
		}
		a, b = timeOrderKey(items[i]["createdDateTime"]), timeOrderKey(items[j]["createdDateTime"])
		if a != b {
			return a < b
		}
		return str(items[i]["id"]) < str(items[j]["id"])
	})
	return items, nil
}
func subtasks(b []byte) ([]Object, error) {
	var env Object
	if json.Unmarshal(b, &env) != nil {
		return nil, microsoft.E(502, "substrate_error")
	}
	val, exists := env["Subtasks"]
	if !exists {
		if str(env["Id"]) != "" {
			return []Object{}, nil
		}
		return nil, microsoft.E(502, "substrate_error")
	}
	if val == nil {
		return []Object{}, nil
	}
	arr, ok := val.([]any)
	if !ok {
		return nil, microsoft.E(502, "substrate_error")
	}
	items := []Object{}
	for _, v := range arr {
		m, ok := v.(map[string]any)
		if !ok || str(m["Id"]) == "" {
			return nil, microsoft.E(502, "substrate_error")
		}
		items = append(items, m)
	}
	return items, nil
}
func integer(q url.Values, key string, def, maxValue int) (int, error) {
	if _, ok := q[key]; !ok {
		return def, nil
	}
	v, err := strconv.Atoi(q.Get(key))
	if err != nil || v < 0 || (key == "limit" && v == 0) || v > maxValue {
		return 0, microsoft.E(400, "invalid_"+key)
	}
	return v, nil
}
