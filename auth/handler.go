package auth

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// Handler is the Fiber adapter around Service. Response shape:
//
//	{ "success": bool, "data": {...} | null, "message": "..." }
//
// This shape is intentionally aligned with what the @asteby/metacore-auth
// frontend package expects, so hosts can adopt the kernel without changing
// their frontend wire format.
type Handler struct {
	service *Service
}

// NewHandler constructs a Handler from an already-configured Service.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// loginRequest is the wire shape for POST /auth/login.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// registerRequest is the wire shape for POST /auth/register. Extra app
// fields (country, org logo, …) are captured into RegisterInput.Extra via
// the generic map.
type registerRequest struct {
	Name             string         `json:"name"`
	Email            string         `json:"email"`
	Password         string         `json:"password"`
	Role             string         `json:"role,omitempty"`
	OrganizationName string         `json:"organization_name,omitempty"`
	Extra            map[string]any `json:"extra,omitempty"`
}

// Login handles POST /auth/login.
func (h *Handler) Login(c *fiber.Ctx) error {
	var req loginRequest
	if err := c.BodyParser(&req); err != nil {
		return respondError(c, fiber.StatusBadRequest, MsgInvalidRequestFormat)
	}
	if req.Email == "" || req.Password == "" {
		return respondError(c, fiber.StatusBadRequest, MsgMissingFields)
	}

	result, err := h.service.Login(c.UserContext(), LoginInput{Email: req.Email, Password: req.Password})
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			return respondError(c, fiber.StatusUnauthorized, MsgInvalidCredentials)
		}
		return respondError(c, fiber.StatusInternalServerError, err.Error())
	}

	return respondLogin(c, fiber.StatusOK, result)
}

// Register handles POST /auth/register.
func (h *Handler) Register(c *fiber.Ctx) error {
	var req registerRequest
	if err := c.BodyParser(&req); err != nil {
		return respondError(c, fiber.StatusBadRequest, MsgInvalidRequestFormat)
	}
	if req.Name == "" || req.Email == "" || req.Password == "" {
		return respondError(c, fiber.StatusBadRequest, MsgMissingFields)
	}

	result, err := h.service.Register(c.UserContext(), RegisterInput{
		Name:             req.Name,
		Email:            req.Email,
		Password:         req.Password,
		Role:             req.Role,
		OrganizationName: req.OrganizationName,
		Extra:            req.Extra,
	})
	if err != nil {
		if errors.Is(err, ErrUserExists) {
			return respondError(c, fiber.StatusConflict, MsgUserExists)
		}
		return respondError(c, fiber.StatusBadRequest, err.Error())
	}

	return respondLogin(c, fiber.StatusCreated, result)
}

// Me handles GET /auth/me. Expects Middleware to have validated the JWT.
func (h *Handler) Me(c *fiber.Ctx) error {
	userID := GetUserID(c)
	if userID == uuid.Nil {
		return respondError(c, fiber.StatusUnauthorized, MsgUnauthorized)
	}

	user, err := h.service.Me(c.UserContext(), userID)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return respondError(c, fiber.StatusNotFound, err.Error())
		}
		return respondError(c, fiber.StatusInternalServerError, err.Error())
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"data":    fiber.Map{"user": user},
	})
}

// Logout handles POST /auth/logout. JWTs are stateless; we just signal success.
func (h *Handler) Logout(c *fiber.Ctx) error {
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": MsgLoggedOut,
	})
}

// Mount registers the standard auth routes on the given router. The middleware
// parameter guards /me and /logout.
func (h *Handler) Mount(router fiber.Router, middleware fiber.Handler) {
	router.Post("/login", h.Login)
	router.Post("/register", h.Register)
	if middleware != nil {
		router.Get("/me", middleware, h.Me)
		router.Post("/logout", middleware, h.Logout)
	} else {
		router.Get("/me", h.Me)
		router.Post("/logout", h.Logout)
	}
}

// respondLogin serializes a LoginResult using the shared wire shape. It does
// NOT include a password field — the User type's JSON tags should hide that.
func respondLogin(c *fiber.Ctx, status int, r *LoginResult) error {
	data := fiber.Map{
		"user":       r.User,
		"token":      r.Token,
		"expires_at": r.ExpiresAt,
	}
	if r.Organization != nil {
		data["organization"] = r.Organization
	}
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
