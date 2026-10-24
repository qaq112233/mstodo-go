package todo

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/qaq112233/mstodo-go/internal/microsoft"
	"github.com/qaq112233/mstodo-go/internal/store"
)

func (s *Service) syncList(ctx context.Context, v store.Snapshot, list string, refresh bool) error {
	state, err := s.Store.Sync(ctx, v.Epoch, list)
	if err != nil {
		return err
	}
	if !refresh && state.Complete && !state.Success.IsZero() && s.Now().Sub(state.Success) < 180*time.Second {
		return nil
	}
	_, err = s.flight(ctx, fmt.Sprintf("sync:%d:%d:%s", v.Epoch, v.Revision, list), func(root context.Context) (any, error) {
		c, cancel := context.WithTimeout(root, 60*time.Second)
		defer cancel()
		state, err := s.Store.Sync(c, v.Epoch, list)
		if err != nil {
			return nil, err
		}
		if !refresh && state.Complete && !state.Success.IsZero() && s.Now().Sub(state.Success) < 180*time.Second {
			return nil, nil
		}
		first := microsoft.GraphBase + "/me/todo/lists/" + url.PathEscape(list) + "/tasks/delta"
		raw := state.Cursor
		if raw == "" {
			if err = s.Store.ResetSync(c, v, list); err != nil {
				return nil, err
			}
			raw = first
		}
		reset := false
		seen := map[string]bool{}
		for pages := 0; pages < 1000; pages++ {
			if seen[raw] {
				return nil, microsoft.E(502, "delta_incomplete")
			}
			seen[raw] = true
			b, e := s.MS.Do(c, v.Epoch, "graph", "GET", raw, nil, "")
			if e != nil {
				var upstream *microsoft.Error
				if errors.As(e, &upstream) && upstream.UpstreamStatus == 410 && !reset {
					reset = true
					if e = s.Store.ResetSync(c, v, list); e != nil {
						return nil, e
					}
					raw = first
					seen = map[string]bool{}
					continue
				}
				mark, cancel := context.WithTimeout(context.WithoutCancel(c), 2*time.Second)
				s.Store.SyncError(mark, v.Epoch, list, microsoft.AsError(e).Code)
				cancel()
				return nil, e
			}
			items, next, delta, e := parsePage(b, false, false)
			if e != nil {
				return nil, e
			}
			cursor := next
			done := false
			if delta != "" {
				cursor = delta
				done = true
			}
			if cursor == "" {
				return nil, microsoft.E(502, "delta_incomplete")
			}
			cursor, e = microsoft.Link(cursor, "graph.microsoft.com")
			if e != nil {
				return nil, e
			}
			if e = s.Store.ApplyPage(c, v, list, items, cursor, done, s.Now()); e != nil {
				return nil, e
			}
			if done {
				return nil, nil
			}
			raw = cursor
		}
		return nil, microsoft.E(502, "delta_incomplete")
	})
	return err
}
func (s *Service) query(ctx context.Context, v store.Snapshot, op Operation, refresh bool) (Result, error) {
	q := op.Query
	limit, err := integer(q, "limit", 50, 200)
	if err != nil {
		return Result{}, err
	}
	offset, err := integer(q, "offset", 0, 1<<31-1)
	if err != nil {
		return Result{}, err
	}
	// Validate before making any upstream call.
	status, importance := q.Get("status"), q.Get("importance")
	if status != "" && !oneOf(status, "notStarted", "inProgress", "completed", "waitingOnOthers", "deferred") {
		return Result{}, microsoft.E(400, "invalid_status")
	}
	if importance != "" && !oneOf(importance, "low", "normal", "high") {
		return Result{}, microsoft.E(400, "invalid_importance")
	}
	for _, key := range []string{"dueFrom", "dueTo"} {
		if val := q.Get(key); val != "" && !ValidDate(val) {
			return Result{}, microsoft.E(400, "invalid_"+key)
		}
	}
	if q.Get("dueFrom") != "" && q.Get("dueTo") != "" && q.Get("dueFrom") > q.Get("dueTo") {
		return Result{}, microsoft.E(400, "invalid_due_range")
	}
	if val := q.Get("modifiedSince"); val != "" {
		if _, err := time.Parse(time.RFC3339Nano, val); err != nil {
			return Result{}, microsoft.E(400, "invalid_modifiedSince")
		}
	}
	lists := []string{}
	seen := map[string]bool{}
	for _, id := range q["listId"] {
		if id == "" {
			return Result{}, microsoft.E(400, "invalid_listId")
		}
		if !seen[id] {
			lists = append(lists, id)
			seen[id] = true
		}
	}
	if len(lists) == 0 {
		res, e := s.lists(ctx, v, refresh)
		if e != nil {
			return Result{}, e
		}
		b, e := asObjects(res.Data)
		if e != nil {
			return Result{}, e
		}
		for _, m := range b {
			lists = append(lists, str(m["id"]))
		}
	}
	earliest := time.Time{}
	for _, id := range lists {
		if err = s.syncList(ctx, v, id, refresh); err != nil {
			if errors.Is(err, store.ErrChanged) {
				return Result{}, err
			}
			e := microsoft.AsError(err)
			e.SyncIncomplete = true
			return Result{}, e
		}
		state, e := s.Store.Sync(ctx, v.Epoch, id)
		if e != nil {
			return Result{}, e
		}
		if !state.Complete {
			return Result{}, &microsoft.Error{Status: 503, Code: "sync_incomplete", SyncIncomplete: true}
		}
		if earliest.IsZero() || state.Success.Before(earliest) {
			earliest = state.Success
		}
	}
	if len(lists) == 0 {
		return Result{Status: 200, Data: Object{"tasks": []Object{}, "total": 0, "limit": limit, "offset": offset, "dataFetchedAt": nil}}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(lists)), ",")
	where := " AND list_id IN (" + placeholders + ")"
	args := []any{}
	for _, id := range lists {
		args = append(args, id)
	}
	if term := q.Get("q"); term != "" {
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(term)
		where += ` AND (json_extract(payload,'$.title') LIKE ? ESCAPE '\' OR json_extract(payload,'$.body.content') LIKE ? ESCAPE '\')`
		args = append(args, "%"+escaped+"%", "%"+escaped+"%")
	}
	for _, f := range []struct{ key, field string }{{"status", "status"}, {"importance", "importance"}} {
		if val := q.Get(f.key); val != "" {
			where += " AND json_extract(payload,'$." + f.field + "')=?"
			args = append(args, val)
		}
	}
	if val := q.Get("dueFrom"); val != "" {
		where += ` AND substr(json_extract(payload,'$.dueDateTime.dateTime'),1,10)>=?`
		args = append(args, val)
	}
	if val := q.Get("dueTo"); val != "" {
		where += ` AND substr(json_extract(payload,'$.dueDateTime.dateTime'),1,10)<=?`
		args = append(args, val)
	}
	if val := q.Get("modifiedSince"); val != "" {
		where += ` AND julianday(json_extract(payload,'$.lastModifiedDateTime'))>=julianday(?)`
		args = append(args, val)
	}
	items, total, err := s.Store.Query(ctx, v.Epoch, where, args, limit, offset)
	if err != nil {
		return Result{}, err
	}
	return Result{Status: 200, Fetched: earliest, Data: Object{"tasks": items, "total": total, "limit": limit, "offset": offset, "dataFetchedAt": earliest.UTC().Format(time.RFC3339Nano)}}, nil
}
func oneOf(v string, allowed ...string) bool {
	for _, s := range allowed {
		if v == s {
			return true
		}
	}
	return false
}
