package todo

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/qaq112233/mstodo-go/internal/microsoft"
	"github.com/qaq112233/mstodo-go/internal/store"
)

func asObjects(v any) ([]Object, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	var out []Object
	e = json.Unmarshal(b, &out)
	return out, e
}
func (s *Service) mutate(ctx context.Context, v store.Snapshot, op Operation) (Result, error) {
	if err := validate(op); err != nil {
		return Result{}, err
	}
	select {
	case s.writes <- struct{}{}:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	defer func() { <-s.writes }()
	current, err := s.Store.Snapshot(ctx)
	if err != nil {
		return Result{}, err
	}
	if current.Epoch != v.Epoch {
		return Result{}, store.ErrChanged
	}
	v = current
	affected := []string{op.ListID}
	if dest := str(op.Args["toListId"]); dest != "" {
		affected = append(affected, dest)
	}
	roster := strings.HasSuffix(op.Kind, "_list")
	if err = s.Store.Invalidate(ctx, v.Epoch, affected, roster); err != nil {
		return Result{}, err
	}
	current, err = s.Store.Snapshot(ctx)
	if err != nil {
		return Result{}, err
	}
	if current.Epoch != v.Epoch {
		return Result{}, store.ErrChanged
	}
	v = current
	result := Result{Status: 200}
	var data Object
	switch op.Kind {
	case "move":
		dest := str(op.Args["toListId"])
		b, e := s.MS.Do(ctx, v.Epoch, "substrate", "PATCH", substrateTaskURL(dest, op.TaskID), Object{"ParentFolderId": dest}, "")
		err = e
		if e == nil {
			var moved Object
			if json.Unmarshal(b, &moved) != nil || str(moved["Id"]) == "" || str(moved["ParentFolderId"]) != dest {
				err = &microsoft.Error{Status: 502, Code: "move_outcome_unknown", Unknown: true}
			} else {
				data = Object{"previousTaskId": op.TaskID, "taskId": moved["Id"], "fromListId": op.ListID, "toListId": dest}
				result.Data = data
			}
		} else if microsoft.AsError(e).Unknown {
			err = &microsoft.Error{Status: 502, Code: "move_outcome_unknown", Unknown: true}
		}
	case "add_myday", "remove_myday":
		fields := Object{"CommittedDay": nil}
		if op.Kind == "add_myday" {
			q := url.Values{}
			if date, ok := op.Args["date"]; ok {
				q.Set("date", str(date))
			}
			d, e := s.date(v, q)
			if e != nil {
				return Result{}, e
			}
			fields = Object{"CommittedDay": d, "PostponedDay": nil, "CommittedOrder": s.Now().UTC().Format("2006-01-02T15:04:05.000Z")}
		}
		var b []byte
		b, err = s.MS.Do(ctx, v.Epoch, "substrate", "PATCH", substrateTaskURL(op.ListID, op.TaskID), fields, "")
		if err == nil {
			err = json.Unmarshal(b, &data)
			if err == nil && (str(data["Id"]) == "" || str(data["Id"]) != op.TaskID) {
				err = &microsoft.Error{Status: 502, Code: "write_outcome_unknown", Unknown: true}
			}
			result.Data = data
		}
	case "reorder_task", "reorder_checklist", "reorder_myday":
		result, err = s.reorder(ctx, v, op)
	default:
		method := "PATCH"
		if strings.HasPrefix(op.Kind, "create_") {
			method = "POST"
			result.Status = 201
		}
		if strings.HasPrefix(op.Kind, "delete_") {
			method = "DELETE"
			result.Status = 204
		}
		var body any = op.Args
		if method == "DELETE" {
			body = nil
		}
		b, e := s.MS.Do(ctx, v.Epoch, "graph", method, graphURL(op), body, op.IfMatch)
		err = e
		if err == nil && method != "DELETE" {
			if e = json.Unmarshal(b, &data); e != nil || str(data["id"]) == "" {
				err = &microsoft.Error{Status: 502, Code: "write_outcome_unknown", Unknown: true}
			}
			result.Data = data
		}
	}
	// Always fence in-flight reads again, including unknown write outcomes. This
	// bounded cleanup survives caller cancellation. Failed local caching must not
	// turn a confirmed remote create into an invitation to create it again.
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cacheErr := s.Store.Invalidate(cleanup, v.Epoch, affected, roster)
	if cacheErr == nil && err == nil {
		switch op.Kind {
		case "create_task", "update_task":
			cacheErr = s.Store.ConfirmTask(cleanup, v.Epoch, op.ListID, op.TaskID, data, false)
		case "delete_task", "move":
			cacheErr = s.Store.ConfirmTask(cleanup, v.Epoch, op.ListID, op.TaskID, nil, true)
		}
	}
	if cacheErr != nil && !errors.Is(cacheErr, store.ErrChanged) {
		s.dirty.Store(true)
		result.Warning = "cache_update_failed; remote write confirmed; do not repeat"
	}
	if err != nil {
		return Result{}, err
	}
	return result, nil
}
func substrateTaskURL(list, task string) string {
	return microsoft.SubstrateBase + "/taskfolders/" + url.PathEscape(list) + "/tasks/" + url.PathEscape(task)
}

func validate(op Operation) error {
	invalid := func() error { return microsoft.E(400, "invalid_fields") }
	switch op.Kind {
	case "move":
		if len(op.Args) != 1 || str(op.Args["toListId"]) == "" || str(op.Args["toListId"]) == op.ListID {
			return invalid()
		}
		return nil
	case "add_myday":
		for key, val := range op.Args {
			if key != "date" || !ValidDate(str(val)) {
				return microsoft.E(400, "invalid_date")
			}
		}
		return nil
	case "remove_myday":
		if len(op.Args) > 0 {
			return invalid()
		}
		return nil
	case "reorder_task", "reorder_checklist", "reorder_myday":
		return validateReorder(op)
	case "delete_list", "delete_task", "delete_checklist", "delete_link":
		return nil
	}
	var allowed []string
	required := ""
	switch op.Kind {
	case "create_list", "update_list":
		allowed = []string{"displayName"}
		required = "displayName"
	case "create_task", "update_task":
		allowed = []string{"title", "body", "importance", "status", "isReminderOn", "categories", "dueDateTime", "startDateTime", "reminderDateTime", "completedDateTime", "recurrence"}
		required = "title"
	case "create_checklist", "update_checklist":
		allowed = []string{"displayName", "isChecked"}
		required = "displayName"
	case "create_link", "update_link":
		allowed = []string{"applicationName", "displayName", "externalId", "webUrl"}
		required = "applicationName"
	default:
		return microsoft.E(404, "not_found")
	}
	if len(op.Args) == 0 {
		return invalid()
	}
	if strings.HasPrefix(op.Kind, "create_") && strings.TrimSpace(str(op.Args[required])) == "" {
		return invalid()
	}
	for key, val := range op.Args {
		if !oneOf(key, allowed...) {
			return invalid()
		}
		switch key {
		case "title", "displayName", "applicationName":
			if strings.TrimSpace(str(val)) == "" {
				return invalid()
			}
		case "externalId":
			if _, ok := val.(string); !ok {
				return invalid()
			}
		case "webUrl":
			u, e := url.Parse(str(val))
			if e != nil || u.Host == "" || !oneOf(u.Scheme, "https", "http") {
				return invalid()
			}
		case "status":
			if !oneOf(str(val), "notStarted", "inProgress", "completed", "waitingOnOthers", "deferred") {
				return invalid()
			}
		case "importance":
			if !oneOf(str(val), "low", "normal", "high") {
				return invalid()
			}
		case "isReminderOn", "isChecked":
			if _, ok := val.(bool); !ok {
				return invalid()
			}
		case "categories":
			b, _ := json.Marshal(val)
			var categories []string
			if val == nil || json.Unmarshal(b, &categories) != nil {
				return invalid()
			}
		case "body":
			m, ok := val.(map[string]any)
			if !ok || !oneOf(str(m["contentType"]), "text", "html") {
				return invalid()
			}
			if _, ok = m["content"].(string); !ok {
				return invalid()
			}
		case "dueDateTime", "startDateTime", "completedDateTime", "reminderDateTime":
			if val == nil {
				continue
			}
			m, ok := val.(map[string]any)
			if !ok || str(m["timeZone"]) == "" || !validDateTime(str(m["dateTime"])) {
				return invalid()
			}
		case "recurrence":
			if val == nil {
				continue
			}
			m, ok := val.(map[string]any)
			if !ok {
				return invalid()
			}
			p, pok := m["pattern"].(map[string]any)
			r, rok := m["range"].(map[string]any)
			if !pok || !rok || !oneOf(str(p["type"]), "daily", "weekly", "absoluteMonthly", "relativeMonthly", "absoluteYearly", "relativeYearly") || !oneOf(str(r["type"]), "endDate", "noEnd", "numbered") {
				return invalid()
			}
		}
	}
	return nil
}
func validDateTime(v string) bool {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02"} {
		if _, e := time.Parse(layout, v); e == nil {
			return true
		}
	}
	return false
}
func validateReorder(op Operation) error {
	pos := str(op.Args["position"])
	if !oneOf(pos, "top", "bottom", "before", "after", "index") {
		return microsoft.E(400, "invalid_position")
	}
	for k := range op.Args {
		if !oneOf(k, "position", "index", "referenceTaskId", "referenceListId", "referenceItemId", "listId", "taskId", "date") {
			return microsoft.E(400, "invalid_fields")
		}
	}
	if pos == "index" {
		n, ok := op.Args["index"].(float64)
		if !ok || n < 1 || n != float64(int(n)) {
			return microsoft.E(400, "invalid_index")
		}
	}
	if pos == "before" || pos == "after" {
		key := "referenceTaskId"
		if op.Kind == "reorder_checklist" {
			key = "referenceItemId"
		}
		if str(op.Args[key]) == "" {
			return microsoft.E(400, "reference_required")
		}
	}
	if d, ok := op.Args["date"]; ok && !ValidDate(str(d)) {
		return microsoft.E(400, "invalid_date")
	}
	if op.Kind == "reorder_myday" && (str(op.Args["listId"]) == "" || str(op.Args["taskId"]) == "") {
		return microsoft.E(400, "invalid_fields")
	}
	return nil
}

// ComputeOrder mirrors the baseline's integer-millisecond midpoint algorithm.
func ComputeOrder(pos string, index int, orders []int64, reference, now int64) (int64, error) {
	sorted := append([]int64{}, orders...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] > sorted[j] })
	midpoint := func(hi, lo int64) (int64, error) {
		mid := lo + (hi-lo)/2
		if mid <= lo || mid >= hi {
			return 0, microsoft.E(409, "order_precision_exhausted")
		}
		return mid, nil
	}
	if len(sorted) == 0 && (pos == "top" || pos == "bottom" || pos == "index") {
		return now, nil
	}
	switch pos {
	case "top":
		return sorted[0] + 60000, nil
	case "bottom":
		return sorted[len(sorted)-1] - 60000, nil
	case "index":
		if index <= 1 {
			return sorted[0] + 60000, nil
		}
		if index > len(sorted) {
			return sorted[len(sorted)-1] - 60000, nil
		}
		return midpoint(sorted[index-2], sorted[index-1])
	case "before":
		above := int64(0)
		found := false
		for _, v := range sorted {
			if v > reference {
				above = v
				found = true
			}
		}
		if !found {
			return reference + 60000, nil
		}
		return midpoint(above, reference)
	case "after":
		for _, v := range sorted {
			if v < reference {
				return midpoint(reference, v)
			}
		}
		return reference - 60000, nil
	}
	return 0, microsoft.E(400, "invalid_position")
}
func orderMS(v any) (int64, bool) {
	t, e := time.Parse(time.RFC3339Nano, str(v))
	return t.UnixMilli(), e == nil
}
func (s *Service) reorder(ctx context.Context, v store.Snapshot, op Operation) (Result, error) {
	var rows []Object
	field, idField := "OrderDateTime", "Id"
	moving := op.TaskID
	reference := str(op.Args["referenceTaskId"])
	switch op.Kind {
	case "reorder_task":
		items, e := s.substrateCollection(ctx, v, microsoft.SubstrateBase+"/taskfolders/"+url.PathEscape(op.ListID)+"/tasks?$top=200", 30, "order_list_truncated", true)
		if e != nil {
			return Result{}, e
		}
		rows = items
	case "reorder_checklist":
		b, e := s.MS.Do(ctx, v.Epoch, "substrate", "GET", substrateTaskURL(op.ListID, op.TaskID)+"?$select=*", nil, "")
		if e != nil {
			return Result{}, e
		}
		items, e := subtasks(b)
		if e != nil {
			return Result{}, e
		}
		rows = items
		moving = op.ItemID
		reference = str(op.Args["referenceItemId"])
	case "reorder_myday":
		q := url.Values{}
		if date, ok := op.Args["date"]; ok {
			q.Set("date", str(date))
		}
		result, e := s.myday(ctx, v, q, true)
		if e != nil {
			return Result{}, e
		}
		b, _ := json.Marshal(result.Data)
		var env struct {
			Tasks []Object `json:"tasks"`
		}
		if e = json.Unmarshal(b, &env); e != nil {
			return Result{}, e
		}
		rows = env.Tasks
		moving = str(op.Args["taskId"])
		op.ListID = str(op.Args["listId"])
		op.TaskID = moving
		field = "committedOrder"
		idField = "id"
	}
	orders := []int64{}
	var ref int64
	found, hasRef := false, false
	for _, m := range rows {
		if op.Kind == "reorder_myday" && strings.EqualFold(str(m["status"]), "completed") {
			continue
		}
		id := str(m[idField])
		isMoving := id == moving
		if op.Kind == "reorder_myday" {
			isMoving = isMoving && str(m["listId"]) == op.ListID
		}
		if isMoving {
			found = true
			continue
		}
		ms, ok := orderMS(m[field])
		if ok {
			orders = append(orders, ms)
		}
		if id == reference {
			if op.Kind != "reorder_myday" || str(op.Args["referenceListId"]) == "" || str(m["listId"]) == str(op.Args["referenceListId"]) {
				ref = ms
				hasRef = ok
			}
		}
	}
	if !found {
		return Result{}, microsoft.E(404, "task_or_item_not_in_order_scope")
	}
	pos := str(op.Args["position"])
	if (pos == "before" || pos == "after") && !hasRef {
		return Result{}, microsoft.E(400, "reference_has_no_order")
	}
	index := 0
	if n, ok := op.Args["index"].(float64); ok {
		index = int(n)
	}
	ms, err := ComputeOrder(pos, index, orders, ref, s.Now().UnixMilli())
	if err != nil {
		return Result{}, err
	}
	value := time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
	raw := substrateTaskURL(op.ListID, op.TaskID)
	wireField := "OrderDateTime"
	if op.Kind == "reorder_checklist" {
		raw = microsoft.SubstrateBase + "/tasks/" + url.PathEscape(op.TaskID) + "/subtasks/" + url.PathEscape(op.ItemID)
	}
	if op.Kind == "reorder_myday" {
		wireField = "CommittedOrder"
	}
	_, err = s.MS.Do(ctx, v.Epoch, "substrate", "PATCH", raw, Object{wireField: value}, "")
	if err != nil {
		return Result{}, err
	}
	return Result{Status: 200, Data: Object{"listId": op.ListID, "taskId": op.TaskID, "itemId": nullable(op.ItemID), "position": pos, "field": wireField, "value": value}}, nil
}
