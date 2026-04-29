package push

import (
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// UserResolver pulls the authenticated user ID from the request. Apps wire
// this to their auth middleware (e.g. auth.GetUserID).
type UserResolver func(c fiber.Ctx) uuid.UUID

// Handler exposes Push endpoints over Fiber.
type Handler struct {
	service  *Service
	resolver UserResolver
}

func NewHandler(service *Service, resolver UserResolver) *Handler {
	return &Handler{service: service, resolver: resolver}
}

// Mount attaches routes to the given router.
//
//	GET  /public-key    (unauthenticated)
//	POST /subscribe     (authenticated)
//	POST /unsubscribe   (authenticated)
//	POST /test          (authenticated)
func (h *Handler) Mount(r fiber.Router) {
	r.Get("/public-key", h.publicKey)
	r.Post("/subscribe", h.subscribe)
	r.Post("/unsubscribe", h.unsubscribe)
	r.Post("/test", h.test)
}

func (h *Handler) publicKey(c fiber.Ctx) error {
	return c.JSON(fiber.Map{"success": true, "data": fiber.Map{"public_key": h.service.PublicKey()}})
}

type subscribeBody struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	DeviceType string `json:"device_type"`
	UserAgent  string `json:"user_agent"`
}

func (h *Handler) subscribe(c fiber.Ctx) error {
	uid := h.resolver(c)
	if uid == uuid.Nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"success": false, "message": "not authenticated"})
	}
	var body subscribeBody
	if err := c.Bind().Body(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": "invalid body"})
	}
	sub, err := h.service.Subscribe(c, uid, SubscriptionInput{
		Endpoint:   body.Endpoint,
		P256DH:     body.Keys.P256DH,
		Auth:       body.Keys.Auth,
		DeviceType: body.DeviceType,
		UserAgent:  body.UserAgent,
	})
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": err.Error()})
	}
	return c.JSON(fiber.Map{"success": true, "data": sub})
}

func (h *Handler) unsubscribe(c fiber.Ctx) error {
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := c.Bind().Body(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": "invalid body"})
	}
	if err := h.service.Unsubscribe(c, body.Endpoint); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"success": false, "message": err.Error()})
	}
	return c.JSON(fiber.Map{"success": true})
}

func (h *Handler) test(c fiber.Ctx) error {
	uid := h.resolver(c)
	if uid == uuid.Nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"success": false, "message": "not authenticated"})
	}
	if err := h.service.Test(c, uid); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"success": false, "message": err.Error()})
	}
	return c.JSON(fiber.Map{"success": true})
}
