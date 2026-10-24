package app_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/qaq112233/mstodo-go/internal/testutil"
)

func TestRESTResourceRoutesAndGraphFields(t *testing.T) {
	var gotMethod, gotPath, gotMatch string
	var gotBody map[string]any
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotMatch = r.Method, r.URL.EscapedPath(), r.Header.Get("If-Match")
		gotBody = nil
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
		}
		if r.Host == "substrate.office.com" {
			io.WriteString(w, `{"Id":"T","Subtasks":[]}`)
			return
		}
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		if strings.HasSuffix(r.URL.Path, "checklistItems") && r.Method == "GET" || strings.HasSuffix(r.URL.Path, "linkedResources") && r.Method == "GET" {
			io.WriteString(w, `{"value":[]}`)
			return
		}
		if r.Method == "POST" {
			w.WriteHeader(201)
		}
		io.WriteString(w, `{"id":"id","displayName":"test"}`)
	})
	front := endpoint(t, f)
	v, _ := f.Store.Snapshot(context.Background())
	auth := v.Settings.Bearer
	for _, tc := range []struct {
		method, path, body, wantMethod, wantPath string
		status                                   int
	}{
		{"POST", "/api/v1/lists", `{"displayName":"清单"}`, "POST", "/v1.0/me/todo/lists", 201},
		{"PATCH", "/api/v1/lists/L", `{"displayName":"改名"}`, "PATCH", "/v1.0/me/todo/lists/L", 200},
		{"DELETE", "/api/v1/lists/L", "", "DELETE", "/v1.0/me/todo/lists/L", 204},
		{"GET", "/api/v1/lists/L%2Fx/tasks/T%2Fx", "", "GET", "/v1.0/me/todo/lists/L%2Fx/tasks/T%2Fx", 200},
		{"POST", "/api/v1/lists/L/tasks", `{"title":"任务","body":{"contentType":"text","content":"正文"},"isReminderOn":true,"recurrence":{"pattern":{"type":"daily","interval":1},"range":{"type":"noEnd","startDate":"2026-10-24"}}}`, "POST", "/v1.0/me/todo/lists/L/tasks", 201},
		{"POST", "/api/v1/lists/L/tasks/T/checklist-items", `{"displayName":"步骤","isChecked":false}`, "POST", "/v1.0/me/todo/lists/L/tasks/T/checklistItems", 201},
		{"GET", "/api/v1/lists/L/tasks/T/checklist-items/I", "", "GET", "/v1.0/me/todo/lists/L/tasks/T/checklistItems/I", 200},
		{"PATCH", "/api/v1/lists/L/tasks/T/checklist-items/I", `{"isChecked":true}`, "PATCH", "/v1.0/me/todo/lists/L/tasks/T/checklistItems/I", 200},
		{"DELETE", "/api/v1/lists/L/tasks/T/checklist-items/I", "", "DELETE", "/v1.0/me/todo/lists/L/tasks/T/checklistItems/I", 204},
		{"GET", "/api/v1/lists/L/tasks/T/linked-resources", "", "GET", "/v1.0/me/todo/lists/L/tasks/T/linkedResources", 200},
		{"POST", "/api/v1/lists/L/tasks/T/linked-resources", `{"applicationName":"test","webUrl":"https://example.test/note"}`, "POST", "/v1.0/me/todo/lists/L/tasks/T/linkedResources", 201},
		{"GET", "/api/v1/lists/L/tasks/T/linked-resources/R", "", "GET", "/v1.0/me/todo/lists/L/tasks/T/linkedResources/R", 200},
		{"PATCH", "/api/v1/lists/L/tasks/T/linked-resources/R", `{"displayName":"链接"}`, "PATCH", "/v1.0/me/todo/lists/L/tasks/T/linkedResources/R", 200},
		{"DELETE", "/api/v1/lists/L/tasks/T/linked-resources/R", "", "DELETE", "/v1.0/me/todo/lists/L/tasks/T/linkedResources/R", 204},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			res := request(t, front.Client(), tc.method, front.URL+tc.path, auth, tc.body)
			if res.StatusCode != tc.status {
				b, _ := io.ReadAll(res.Body)
				t.Fatal(res.StatusCode, string(b))
			}
			if gotMethod != tc.wantMethod || gotPath != tc.wantPath {
				t.Fatal("incorrect upstream path", gotMethod, gotPath)
			}
		})
	}
	req, _ := http.NewRequest("PATCH", front.URL+"/api/v1/lists/L/tasks/T", strings.NewReader(`{"title":"etag"}`))
	req.Header.Set("Authorization", "Bearer "+auth)
	req.Header.Set("If-Match", "etag-1")
	res, e := front.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 200 || gotMatch != "etag-1" || gotBody["title"] != "etag" {
		t.Fatal("ETag or patch body not passed through")
	}
}
