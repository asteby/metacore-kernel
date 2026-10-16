package apiroute

import (
	"errors"
	"net/http"

	"github.com/gofiber/fiber/v3"
)

// PrincipalResolver resolves the caller of a gateway request: an API token
// (apitoken.PrincipalResolver), a customer session, a staff JWT, or an
// anonymous principal whose organization comes from the domain. Returning an
// *Error (ErrUnauthorized, RateLimited(n), …) answers with it; any other
// error answers 401.
type PrincipalResolver func(c fiber.Ctx) (Principal, error)

// Handler is the optional Fiber adapter over a Gateway.
type Handler struct {
	gw      *Gateway
	resolve PrincipalResolver
}

// NewHandler builds the adapter. resolve may be nil: every request is then
// anonymous with no organization (only useful in tests).
func NewHandler(gw *Gateway, resolve PrincipalResolver) *Handler {
	return &Handler{gw: gw, resolve: resolve}
}

// Mount serves every method and path under r, e.g.
//
//	h.Mount(app.Group("/api/gw"))   // GET /api/gw/storefront/products/:handle
func (h *Handler) Mount(r fiber.Router) {
	r.All("/*", h.Serve) // also matches the bare mount path

}

// Serve handles one request: it resolves the principal, hands the gateway the
// path below the mount (the "*" wildcard), query, headers and body, and
// writes the Response — or the ErrorResponse of a failure.
func (h *Handler) Serve(c fiber.Ctx) error {
	var p Principal
	if h.resolve != nil {
		rp, err := h.resolve(c)
		if err != nil {
			var typed *Error
			if !errors.As(err, &typed) {
				err = ErrUnauthorized
			}
			return WriteResponse(c, ErrorResponse(err))
		}
		p = rp
	}
	body := c.Body()
	if int64(len(body)) > h.gw.MaxBodyBytes() {
		return WriteResponse(c, ErrorResponse(ErrPayloadTooLarge))
	}
	in := Inbound{
		Method:    c.Method(),
		Path:      "/" + c.Params("*"),
		Query:     map[string][]string{},
		Headers:   http.Header{},
		Body:      body,
		Principal: p,
	}
	for k, v := range c.Request().URI().QueryArgs().All() {
		key := string(k)
		in.Query[key] = append(in.Query[key], string(v))
	}
	for k, v := range c.Request().Header.All() {
		in.Headers.Add(string(k), string(v))
	}
	resp, err := h.gw.Handle(c.Context(), in)
	if err != nil {
		return WriteResponse(c, ErrorResponse(err))
	}
	return WriteResponse(c, resp)
}

// WriteResponse writes a Response to the Fiber context (no body for HEAD).
func WriteResponse(c fiber.Ctx, resp Response) error {
	for k, v := range resp.Headers {
		c.Set(k, v)
	}
	c.Status(resp.Status)
	if c.Method() == fiber.MethodHead {
		return nil
	}
	if b := resp.BodyBytes(); len(b) > 0 {
		return c.Send(b)
	}
	return nil
}
