package outbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

func TestHandler_ScopedListGetRetry(t *testing.T) {
	s := newService(t, sqliteTestDB(t).open(t), Config{MaxAttempts: 1})
	ctx := context.Background()
	orgA, orgB := uuid.New(), uuid.New()
	mine, _ := s.Enqueue(ctx, "erp", nil, WithOrg(orgA))
	theirs, _ := s.Enqueue(ctx, "erp", nil, WithOrg(orgB))

	app := fiber.New()
	NewHandler(s, func(fiber.Ctx) (*uuid.UUID, error) { return &orgA, nil }).Mount(app.Group("/outbox"))
	call := func(method, path string) (int, map[string]any) {
		resp, err := app.Test(httptest.NewRequest(method, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return resp.StatusCode, out
	}

	status, body := call(http.MethodGet, "/outbox/jobs")
	if status != 200 || body["meta"].(map[string]any)["total"].(float64) != 1 {
		t.Fatalf("list = %d %v", status, body)
	}
	if status, _ := call(http.MethodGet, "/outbox/jobs?status=bogus"); status != 400 {
		t.Fatalf("bad status filter = %d", status)
	}
	if status, _ := call(http.MethodGet, "/outbox/jobs/"+mine.ID.String()); status != 200 {
		t.Fatalf("get own = %d", status)
	}
	if status, _ := call(http.MethodGet, "/outbox/jobs/"+theirs.ID.String()); status != 404 {
		t.Fatalf("other org's job must read as 404, got %d", status)
	}
	if status, _ := call(http.MethodPost, "/outbox/jobs/"+theirs.ID.String()+"/retry"); status != 404 {
		t.Fatalf("retry other org = %d", status)
	}

	// Dead-letter mine, then retry it over HTTP.
	s.Register("erp", func(context.Context, Job) error { return Permanent(io.ErrUnexpectedEOF) })
	start(t, s)
	waitFor(t, "dead", func() bool { return jobStatus(t, s, mine.ID).Status == StatusDead })
	s.Shutdown(ctx)
	status, body = call(http.MethodPost, "/outbox/jobs/"+mine.ID.String()+"/retry")
	if status != 200 || body["data"].(map[string]any)["status"] != "pending" {
		t.Fatalf("retry = %d %v", status, body)
	}
}
