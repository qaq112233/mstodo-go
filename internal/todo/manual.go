package todo

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/qaq112233/mstodo-go/internal/microsoft"
	"github.com/qaq112233/mstodo-go/internal/store"
)

func (s *Service) manual(ctx context.Context, v store.Snapshot, op Operation, refresh bool) (Result, error) {
	for _, key := range []string{"q", "importance", "dueFrom", "dueTo", "modifiedSince"} {
		if op.Query.Get(key) != "" {
			return Result{}, microsoft.E(400, "unsupported_manual_filter")
		}
	}
	limit, err := integer(op.Query, "limit", 50, 200)
	if err != nil {
		return Result{}, err
	}
	offset, err := integer(op.Query, "offset", 0, 1<<31-1)
	if err != nil {
		return Result{}, err
	}
	status := op.Query.Get("status")
	if status != "" && !oneOf(status, "notStarted", "inProgress", "completed", "waitingOnOthers", "deferred") {
		return Result{}, microsoft.E(400, "invalid_status")
	}
	res, err := s.cached(ctx, v, "manual:"+op.ListID, refresh, 60*time.Second, func(c context.Context, v store.Snapshot) (any, error) {
		rows, e := s.substrateCollection(c, v, microsoft.SubstrateBase+"/taskfolders/"+url.PathEscape(op.ListID)+"/tasks?$top=200", 30, "order_list_truncated", true)
		if e != nil {
			return nil, e
		}
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := timeOrderKey(rows[i]["OrderDateTime"]), timeOrderKey(rows[j]["OrderDateTime"])
			if a != b {
				return a > b
			}
			return false // preserve the upstream order when no manual key exists
		})
		tasks := []Object{}
		for _, row := range rows {
			m := Object{"id": row["Id"], "listId": op.ListID, "title": row["Subject"], "status": lowerFirst(str(row["Status"])), "importance": strings.ToLower(str(row["Importance"])), "orderDateTime": row["OrderDateTime"], "categories": row["Categories"], "isReminderOn": row["IsReminderOn"], "hasAttachments": row["HasAttachments"], "createdDateTime": row["CreatedDateTime"], "lastModifiedDateTime": row["LastModifiedDateTime"]}
			for _, key := range []string{"DueDateTime", "StartDateTime", "CompletedDateTime", "ReminderDateTime"} {
				dt := dateTime(row[key])
				if dt == nil {
					m[lowerFirst(key)] = nil
				} else {
					zone := "UTC"
					if obj, ok := row[key].(map[string]any); ok && str(obj["TimeZone"]) != "" {
						zone = str(obj["TimeZone"])
					}
					m[lowerFirst(key)] = Object{"dateTime": dt, "timeZone": zone}
				}
			}
			if body, ok := row["Body"].(map[string]any); ok {
				m["body"] = Object{"content": body["Content"], "contentType": strings.ToLower(str(body["ContentType"]))}
			}
			tasks = append(tasks, m)
		}
		return tasks, nil
	})
	if err != nil {
		return Result{}, err
	}
	b, _ := json.Marshal(res.Data)
	var rows []Object
	if err = json.Unmarshal(b, &rows); err != nil {
		return Result{}, err
	}
	filtered := []Object{}
	for _, row := range rows {
		if status == "" || str(row["status"]) == status {
			filtered = append(filtered, row)
		}
	}
	total := len(filtered)
	start := min(offset, total)
	end := min(start+limit, total)
	res.Data = Object{"tasks": filtered[start:end], "total": total, "limit": limit, "offset": offset, "dataFetchedAt": res.Fetched.UTC().Format(time.RFC3339Nano)}
	return res, nil
}
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
