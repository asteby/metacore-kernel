package ws

import (
	"log"

	"github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// IDExtractor converts the raw Locals value set by the host's auth
// middleware into the hub's key type. Return ok=false to reject the
// connection (no/invalid identity).
type IDExtractor[K comparable] func(raw any) (id K, ok bool)

// UUIDExtractor is the historical identity extraction: accepts uuid.UUID or
// its string form.
func UUIDExtractor(raw any) (uuid.UUID, bool) {
	switch v := raw.(type) {
	case uuid.UUID:
		return v, v != uuid.Nil
	case string:
		if parsed, err := uuid.Parse(v); err == nil {
			return parsed, parsed != uuid.Nil
		}
	}
	return uuid.Nil, false
}

// HandlerOf returns a Fiber handler that upgrades to WebSocket and registers
// the client with the hub. userIDKey is the Locals key set by auth middleware
// (typically "user_id"); extract adapts its raw value to the hub's key type.
func HandlerOf[K comparable](hub *HubOf[K], userIDKey string, extract IDExtractor[K]) fiber.Handler {
	return websocket.New(func(c *websocket.Conn) {
		// gofiber/websocket propagates Locals from the Fiber ctx to the
		// websocket.Conn — but only values set BEFORE websocket.New runs.
		// The auth middleware sets user_id before the upgrade, so it's available.
		userID, ok := extract(c.Locals(userIDKey))
		if !ok {
			log.Printf("ws: rejected — no valid %s in Locals (type: %T, value: %v)", userIDKey, c.Locals(userIDKey), c.Locals(userIDKey))
			c.WriteMessage(websocket.CloseMessage, []byte("auth required"))
			c.Close()
			return
		}

		log.Printf("ws: user %v connected", userID)
		client := &ClientOf[K]{Hub: hub, conn: c, send: make(chan []byte, 1024), UserID: userID}
		hub.register <- client

		go client.writePump()
		client.readPump()
	})
}

// Handler is the historical uuid-keyed handler.
func Handler(hub *Hub, userIDKey string) fiber.Handler {
	return HandlerOf(hub, userIDKey, UUIDExtractor)
}

// MountOf registers the WebSocket route on the given router with a custom
// identity extractor. The auth middleware MUST run before the upgrade to
// populate Locals[userIDKey].
func MountOf[K comparable](router fiber.Router, hub *HubOf[K], authMiddleware fiber.Handler, userIDKey string, extract IDExtractor[K]) {
	router.Use("/ws", func(c fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})
	router.Get("/ws", authMiddleware, HandlerOf(hub, userIDKey, extract))
}

// Mount registers the WebSocket route with the historical uuid identity.
func Mount(router fiber.Router, hub *Hub, authMiddleware fiber.Handler, userIDKey string) {
	MountOf(router, hub, authMiddleware, userIDKey, UUIDExtractor)
}
