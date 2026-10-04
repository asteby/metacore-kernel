package dynamic

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestHandleError_CorrelationID(t *testing.T) {
	h := &Handler{}
	app := fiber.New()
	app.Get("/ctx", func(c fiber.Ctx) error {
		c.SetContext(WithCorrelationID(c.Context(), "corr-ctx"))
		return h.handleError(c, ErrForbidden)
	})
	app.Get("/header", func(c fiber.Ctx) error {
		return h.handleError(c, ErrRecordNotFound)
	})
	app.Get("/alt", func(c fiber.Ctx) error {
		return h.handleError(c, ErrInvalidInput)
	})
	app.Get("/none", func(c fiber.Ctx) error {
		return h.handleError(c, ErrInvalidInput)
	})
	app.Get("/validation", func(c fiber.Ctx) error {
		c.SetContext(WithCorrelationID(c.Context(), "corr-val"))
		ve := NewValidationError()
		ve.AddMessage("name", "required")
		return h.handleError(c, ve)
	})
	app.Get("/prefers-context", func(c fiber.Ctx) error {
		c.SetContext(WithCorrelationID(c.Context(), "from-ctx"))
		return h.handleError(c, ErrForbidden)
	})

	cases := []struct {
		path       string
		header     string
		headerVal  string
		wantStatus int
		wantID     string
		omit       bool
	}{
		{path: "/ctx", wantStatus: fiber.StatusForbidden, wantID: "corr-ctx"},
		{path: "/header", header: "X-Request-ID", headerVal: "from-header", wantStatus: fiber.StatusNotFound, wantID: "from-header"},
		{path: "/alt", header: "X-Correlation-ID", headerVal: "from-alt", wantStatus: fiber.StatusBadRequest, wantID: "from-alt"},
		{path: "/none", wantStatus: fiber.StatusBadRequest, omit: true},
		{path: "/validation", wantStatus: fiber.StatusUnprocessableEntity, wantID: "corr-val"},
		{path: "/prefers-context", header: "X-Request-ID", headerVal: "from-header", wantStatus: fiber.StatusForbidden, wantID: "from-ctx"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest("GET", tc.path, nil)
			if tc.header != "" {
				req.Header.Set(tc.header, tc.headerVal)
			}
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, body=%s", resp.StatusCode, raw)
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got, ok := body["correlation_id"].(string)
			if tc.omit {
				if ok {
					t.Fatalf("correlation_id = %q, want omitted", got)
				}
				return
			}
			if got != tc.wantID {
				t.Fatalf("correlation_id = %q, want %q (body %s)", got, tc.wantID, raw)
			}
		})
	}
}
