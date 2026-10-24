package mcpserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qaq112233/mstodo-go/internal/httpapi"
	"github.com/qaq112233/mstodo-go/internal/microsoft"
	"github.com/qaq112233/mstodo-go/internal/todo"
)

type spec struct {
	name, kind, description string
	read                    bool
	required                []string
	properties              map[string]any
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "description": description}
}
func boolProp() map[string]any { return map[string]any{"type": "boolean"} }
func Server(s *todo.Service) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "mstodo-go", Version: "0.1.0"}, nil)
	specs := []spec{
		{name: "list_my_day_tasks", kind: "myday", description: "读取完整直接 My Day feed，默认保留已完成项；180 秒缓存，失败返回错误。", read: true},
		{name: "list_lists", kind: "lists", read: true}, {name: "create_list", kind: "create_list"},
		{name: "get_list", kind: "get_list", read: true}, {name: "update_list", kind: "update_list"}, {name: "delete_list", kind: "delete_list"},
		{name: "list_tasks", kind: "list_tasks", read: true}, {name: "create_task", kind: "create_task"}, {name: "get_task", kind: "get_task", read: true}, {name: "update_task", kind: "update_task"}, {name: "delete_task", kind: "delete_task"},
		{name: "query_tasks", kind: "query", read: true}, {name: "search_tasks", kind: "query", read: true},
		{name: "add_to_my_day", kind: "add_myday"}, {name: "remove_from_my_day", kind: "remove_myday"},
		{name: "move_task", kind: "move", description: "将原任务重挂到另一清单，保留其步骤、附件和链接；返回新 taskId。结果不明时不要重复操作。"},
		{name: "list_checklist_items", kind: "list_checklist", read: true}, {name: "create_checklist_item", kind: "create_checklist"}, {name: "get_checklist_item", kind: "get_checklist", read: true}, {name: "update_checklist_item", kind: "update_checklist"}, {name: "delete_checklist_item", kind: "delete_checklist"},
		{name: "list_linked_resources", kind: "list_links", read: true}, {name: "create_linked_resource", kind: "create_link"}, {name: "get_linked_resource", kind: "get_link", read: true}, {name: "update_linked_resource", kind: "update_link"}, {name: "delete_linked_resource", kind: "delete_link"},
		{name: "list_tasks_by_manual_order", kind: "list_tasks", read: true}, {name: "reorder_task", kind: "reorder_task"}, {name: "reorder_checklist_item", kind: "reorder_checklist"}, {name: "reorder_my_day_task", kind: "reorder_myday"},
		{name: "whoami", kind: "status", read: true}, {name: "sync_status", kind: "status", read: true},
	}
	for _, sp := range specs {
		props := map[string]any{}
		required := []string{}
		add := func(k string, p any, req bool) {
			props[k] = p
			if req {
				required = append(required, k)
			}
		}
		if sp.read {
			add("refresh", boolProp(), false)
		}
		if sp.kind == "myday" || sp.kind == "add_myday" || sp.kind == "reorder_myday" {
			add("date", stringProp("YYYY-MM-DD；默认使用设置时区的今天"), false)
		}
		if sp.kind == "myday" {
			add("include_completed", boolProp(), false)
		}
		if sp.kind != "myday" && sp.kind != "lists" && sp.kind != "create_list" && sp.kind != "query" && sp.kind != "status" {
			add("list", stringProp("微软清单 ID（首版不支持别名或名称解析）"), true)
		}
		if strings.Contains(sp.kind, "task") && sp.kind != "create_task" && sp.kind != "list_tasks" || strings.Contains(sp.kind, "checklist") || strings.Contains(sp.kind, "link") || sp.kind == "move" || sp.kind == "add_myday" || sp.kind == "remove_myday" || sp.kind == "reorder_myday" {
			add("task_id", stringProp("微软任务 ID"), true)
		}
		if sp.kind == "get_checklist" || sp.kind == "update_checklist" || sp.kind == "delete_checklist" || sp.kind == "reorder_checklist" {
			add("item_id", stringProp("步骤 ID"), true)
		}
		if sp.kind == "get_link" || sp.kind == "update_link" || sp.kind == "delete_link" {
			add("resource_id", stringProp("关联链接 ID"), true)
		}
		if sp.kind == "query" || sp.kind == "list_tasks" {
			add("limit", map[string]any{"type": "integer", "minimum": 1, "maximum": 200}, false)
			add("offset", map[string]any{"type": "integer", "minimum": 0, "maximum": 2147483647}, false)
			for _, key := range []string{"q", "status", "importance", "dueFrom", "dueTo", "modifiedSince"} {
				props[key] = map[string]any{"type": "string"}
			}
			if sp.kind == "query" {
				add("list_ids", map[string]any{"type": "array", "items": stringProp("清单 ID")}, false)
			} else {
				props["order"] = map[string]any{"type": "string", "enum": []string{"manual"}}
			}
			if sp.name == "search_tasks" {
				required = append(required, "q")
			}
		}
		if sp.kind == "create_list" || sp.kind == "update_list" {
			add("displayName", stringProp("清单名称"), sp.kind == "create_list")
		}
		if sp.kind == "create_task" || sp.kind == "update_task" {
			add("title", stringProp("任务标题"), sp.kind == "create_task")
			props["body"] = map[string]any{"type": []string{"string", "object"}, "description": "纯文本，或 Graph {contentType,content} 对象"}
			for _, key := range []string{"due", "start", "reminder", "completed_date"} {
				props[key] = map[string]any{"type": []string{"string", "null"}, "description": "UTC ISO 日期时间；null 清空，省略不修改"}
			}
			for _, key := range []string{"dueDateTime", "startDateTime", "reminderDateTime", "completedDateTime", "recurrence"} {
				props[key] = map[string]any{"type": []string{"object", "null"}}
			}
			props["status"] = map[string]any{"type": "string", "enum": []string{"notStarted", "inProgress", "completed", "waitingOnOthers", "deferred"}}
			props["importance"] = map[string]any{"type": "string", "enum": []string{"low", "normal", "high"}}
			props["isReminderOn"] = boolProp()
			props["categories"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
			if sp.kind == "update_task" {
				add("if_match", stringProp("读取任务的 @odata.etag；避免覆盖并发编辑"), false)
			}
		}
		if sp.kind == "create_checklist" || sp.kind == "update_checklist" {
			add("displayName", stringProp("步骤内容"), sp.kind == "create_checklist")
			props["isChecked"] = boolProp()
		}
		if sp.kind == "create_link" || sp.kind == "update_link" {
			for _, k := range []string{"applicationName", "displayName", "externalId", "webUrl"} {
				add(k, stringProp(k), sp.kind == "create_link" && k == "applicationName")
			}
		}
		if sp.kind == "move" {
			add("to_list", stringProp("目标清单 ID"), true)
		}
		if strings.HasPrefix(sp.kind, "reorder_") {
			add("position", map[string]any{"type": "string", "enum": []string{"top", "bottom", "before", "after", "index"}}, true)
			props["index"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}
			if sp.kind == "reorder_checklist" {
				add("reference_item_id", stringProp("before/after 的步骤 ID"), false)
			} else {
				add("reference_task_id", stringProp("before/after 的任务 ID"), false)
			}
			if sp.kind == "reorder_myday" {
				add("reference_list_id", stringProp("引用任务所属清单 ID"), false)
			}
		}
		destructive := strings.HasPrefix(sp.kind, "delete_") || strings.HasPrefix(sp.kind, "update_") || sp.kind == "move"
		desc := sp.description
		if desc == "" {
			desc = sp.name + "；与 REST 共用业务逻辑、缓存和错误码。读操作按需刷新，空闲不读取任务。"
		}
		if strings.HasPrefix(sp.kind, "create_") {
			desc += " 创建结果未确认时不会自动重试，请先核对微软网页。"
		}
		tool := &mcp.Tool{Name: sp.name, Description: desc, InputSchema: map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: sp.read, DestructiveHint: &destructive}}
		mcp.AddTool(server, tool, func(ctx context.Context, _ *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, any, error) {
			for short, full := range map[string]string{"due": "dueDateTime", "start": "startDateTime", "reminder": "reminderDateTime", "completed_date": "completedDateTime"} {
				_, a := input[short]
				_, b := input[full]
				if a && b {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: `{"error":"duplicate_field_alias"}`}}}, nil, nil
				}
			}
			op := adapt(sp, input)
			res, err := s.Call(ctx, op)
			if err != nil {
				e := microsoft.AsError(err)
				b, _ := json.Marshal(e)
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
			}
			if res.Status == 204 {
				return nil, todo.Object{"ok": true}, nil
			}
			if res.Warning != "" {
				return nil, todo.Object{"result": res.Data, "cacheWarning": res.Warning}, nil
			}
			return nil, res.Data, nil
		})
	}
	return server
}
func Handler(s *todo.Service, base string) http.Handler {
	server := Server(s)
	u, _ := url.Parse(base)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 1 << 20, PropagateRequestCancellation: true, DisableLocalhostProtection: true})
	// A loopback reverse proxy may preserve the configured public Host. Accept
	// that exact origin in addition to loopback, while retaining rebinding checks.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, e := net.SplitHostPort(host); e == nil {
			host = h
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if !strings.EqualFold(r.Host, u.Host) && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			httpapi.Failure(w, microsoft.E(403, "host_rejected"))
			return
		}
		handler.ServeHTTP(w, r)
	})
}
func adapt(sp spec, in map[string]any) todo.Operation {
	text := func(key string) string { s, _ := in[key].(string); return s }
	op := todo.Operation{Kind: sp.kind, ListID: text("list"), TaskID: text("task_id"), ItemID: text("item_id"), Args: todo.Object{}, Query: url.Values{}, IfMatch: text("if_match")}
	if op.ItemID == "" {
		op.ItemID = text("resource_id")
	}
	for k, v := range in {
		if sp.read {
			switch k {
			case "list", "task_id", "item_id", "resource_id":
				continue
			case "list_ids":
				for _, id := range v.([]any) {
					op.Query.Add("listId", id.(string))
				}
				continue
			case "include_completed":
				k = "includeCompleted"
			}
			switch val := v.(type) {
			case string:
				op.Query.Set(k, val)
			case bool:
				op.Query.Set(k, strconv.FormatBool(val))
			case float64:
				op.Query.Set(k, strconv.Itoa(int(val)))
			}
		} else {
			switch k {
			case "list", "task_id", "item_id", "resource_id", "if_match":
				continue
			case "to_list":
				k = "toListId"
			case "reference_task_id":
				k = "referenceTaskId"
			case "reference_item_id":
				k = "referenceItemId"
			case "reference_list_id":
				k = "referenceListId"
			case "body":
				if t, ok := v.(string); ok {
					v = todo.Object{"contentType": "text", "content": t}
				}
			case "due", "start", "reminder", "completed_date":
				fields := map[string]string{"due": "dueDateTime", "start": "startDateTime", "reminder": "reminderDateTime", "completed_date": "completedDateTime"}
				k = fields[k]
				if v != nil {
					v = todo.Object{"dateTime": v, "timeZone": "UTC"}
				}
			}
			op.Args[k] = v
		}
	}
	if sp.name == "list_tasks_by_manual_order" {
		op.Query.Set("order", "manual")
	}
	if sp.kind == "reorder_myday" {
		op.Args["listId"] = op.ListID
		op.Args["taskId"] = op.TaskID
	}
	return op
}
