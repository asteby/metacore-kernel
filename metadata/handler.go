package metadata

import (
	"errors"

	"github.com/gofiber/fiber/v2"
)

// Handler is the Fiber adapter around Service. Response shape matches what
// the @asteby/metacore-runtime-react frontend already consumes:
//
//	{ "success": true,  "data": { ... } }
//	{ "success": false, "message": "..." }
//
// Keep it stable — changing this shape is a MAJOR bump.
type Handler struct {
	service *Service
}

// NewHandler constructs a Handler from an already-configured Service.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// GetTable handles GET /metadata/table/:model.
func (h *Handler) GetTable(c *fiber.Ctx) error {
	modelKey := c.Params(paramModel)
	if modelKey == "" {
		return respondError(c, fiber.StatusBadRequest, MsgModelNotFound)
	}

	meta, err := h.service.GetTable(c.UserContext(), modelKey)
	if err != nil {
		return h.respondServiceError(c, err)
	}
	return respondData(c, fiber.StatusOK, meta)
}

// GetModal handles GET /metadata/modal/:model.
func (h *Handler) GetModal(c *fiber.Ctx) error {
	modelKey := c.Params(paramModel)
	if modelKey == "" {
		return respondError(c, fiber.StatusBadRequest, MsgModelNotFound)
	}

	meta, err := h.service.GetModal(c.UserContext(), modelKey)
	if err != nil {
		return h.respondServiceError(c, err)
	}
	return respondData(c, fiber.StatusOK, meta)
}

// GetAll handles GET /metadata/all.
func (h *Handler) GetAll(c *fiber.Ctx) error {
	all, err := h.service.GetAll(c.UserContext())
	if err != nil {
		return h.respondServiceError(c, err)
	}
	return respondData(c, fiber.StatusOK, all)
}

// Mount registers the three metadata endpoints on router. Any middleware
// supplied is applied to ALL three routes (typically auth.Middleware). Pass
// no middleware to expose the endpoints unauthenticated — handy for public
// docs apps or local dev.
//
// Routes:
//
//	GET /table/:model
//	GET /modal/:model
//	GET /all
//
// Callers control the prefix by scoping router themselves, e.g.
// `h.Mount(app.Group("/api/metadata"), authMW)`.
func (h *Handler) Mount(router fiber.Router, middleware ...fiber.Handler) {
	handlers := make([]fiber.Handler, 0, len(middleware)+1)
	for _, mw := range middleware {
		if mw != nil {
			handlers = append(handlers, mw)
		}
	}

	tableChain := append(append([]fiber.Handler(nil), handlers...), h.GetTable)
	modalChain := append(append([]fiber.Handler(nil), handlers...), h.GetModal)
	allChain := append(append([]fiber.Handler(nil), handlers...), h.GetAll)

	router.Get("/table/:"+paramModel, tableChain...)
	router.Get("/modal/:"+paramModel, modalChain...)
	router.Get("/all", allChain...)
}

// respondServiceError maps a service-layer error onto the right HTTP status.
// Unknown errors become 500 so we never leak internal detail by accident.
func (h *Handler) respondServiceError(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, ErrModelNotFound):
		return respondError(c, fiber.StatusNotFound, err.Error())
	case errors.Is(err, ErrMetadataInvalid):
		return respondError(c, fiber.StatusUnprocessableEntity, err.Error())
	default:
		return respondError(c, fiber.StatusInternalServerError, MsgInternalError)
	}
}

func respondData(c *fiber.Ctx, status int, data interface{}) error {
	return c.Status(status).JSON(fiber.Map{
		"success": true,
		"data":    data,
	})
}

func respondError(c *fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(fiber.Map{
		"success": false,
		"message": msg,
	})
}
