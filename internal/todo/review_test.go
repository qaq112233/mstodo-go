package todo_test

import (
	"context"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/qaq112233/mstodo-go/internal/microsoft"
	"github.com/qaq112233/mstodo-go/internal/store"
	"github.com/qaq112233/mstodo-go/internal/testutil"
	"github.com/qaq112233/mstodo-go/internal/todo"
)

type tokenProviderFunc func(context.Context, int64, string, string) (string, string, error)

func (f tokenProviderFunc) Get(ctx context.Context, epoch int64, resource, rejected string) (string, string, error) {
	return f(ctx, epoch, resource, rejected)
}

func TestAccountChangeDoesNotReplayOperations(t *testing.T) {
	for _, op := range []todo.Operation{
		{Kind: "myday"},
		{Kind: "create_list", Args: todo.Object{"displayName": "old account request"}},
	} {
		t.Run(op.Kind, func(t *testing.T) {
			var calls, tokenCalls atomic.Int32
			f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method == "POST" {
					w.WriteHeader(201)
					writeJSON(w, todo.Object{"id": "new-account-list"})
				} else if r.Host == "graph.microsoft.com" {
					writeJSON(w, todo.Object{"value": []any{}})
				} else {
					writeJSON(w, todo.Object{"Value": []any{}})
				}
			})
			f.MS.Tokens = tokenProviderFunc(func(ctx context.Context, epoch int64, resource, rejected string) (string, string, error) {
				if tokenCalls.Add(1) == 1 {
					a, err := f.Store.Account(ctx, epoch)
					if err != nil {
						return "", "", err
					}
					a.ID = "new-account"
					if err = f.Store.InstallAccount(ctx, epoch, a); err != nil {
						return "", "", err
					}
					// Token refresh detects an authorization change before sending.
					return "", "", store.ErrChanged
				}
				return f.Tokens.Get(ctx, epoch, resource, rejected)
			})
			_, err := f.Todo.Call(context.Background(), op)
			if err == nil || microsoft.AsError(err).Code != "account_changed" {
				t.Fatalf("old operation survived authorization change: %v", err)
			}
			if calls.Load() != 0 {
				t.Fatalf("old operation sent to new account: %d requests", calls.Load())
			}
		})
	}
}

func TestTimestampOrderingUsesInstants(t *testing.T) {
	for _, mode := range []string{"myday", "manual", "checklist"} {
		t.Run(mode, func(t *testing.T) {
			f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Host == "graph.microsoft.com" {
					items := []any{}
					if mode == "checklist" {
						items = []any{todo.Object{"id": "whole"}, todo.Object{"id": "fraction"}, todo.Object{"id": "offset"}}
					}
					writeJSON(w, todo.Object{"value": items})
					return
				}
				items := []any{
					todo.Object{"Id": "whole", "ParentFolderId": "L", "CommittedOrder": "2026-10-24T02:00:00Z", "OrderDateTime": "2026-10-24T02:00:00Z"},
					todo.Object{"Id": "fraction", "ParentFolderId": "L", "CommittedOrder": "2026-10-24T02:00:00.001Z", "OrderDateTime": "2026-10-24T02:00:00.001Z"},
					todo.Object{"Id": "offset", "ParentFolderId": "L", "CommittedOrder": "2026-10-24T03:00:00+02:00", "OrderDateTime": "2026-10-24T03:00:00+02:00"},
				}
				if mode == "checklist" {
					writeJSON(w, todo.Object{"Id": "T", "Subtasks": items})
				} else {
					writeJSON(w, todo.Object{"Value": items})
				}
			})
			op := todo.Operation{Kind: "myday"}
			if mode == "manual" {
				op = todo.Operation{Kind: "list_tasks", ListID: "L", Query: url.Values{"order": {"manual"}}}
			} else if mode == "checklist" {
				op = todo.Operation{Kind: "list_checklist", ListID: "L", TaskID: "T"}
			}
			res, err := f.Todo.Call(context.Background(), op)
			if err != nil {
				t.Fatal(err)
			}
			var rows []todo.Object
			switch mode {
			case "myday":
				rows = tasks(t, res)
			case "manual":
				rows = res.Data.(todo.Object)["tasks"].([]todo.Object)
			case "checklist":
				rows = res.Data.([]todo.Object)
			}
			for i, want := range []string{"fraction", "whole", "offset"} {
				if rows[i]["id"] != want {
					t.Fatalf("incorrect time order: %#v", rows)
				}
			}
		})
	}
}
