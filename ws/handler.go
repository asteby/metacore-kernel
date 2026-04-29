package ws

import (
	"log"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/contrib/v3/websocket"
	"github.com/google/uuid"
)

// Handler returns a Fiber handler that upgrades to WebSocket and registers
// the client with the hub. userIDKey is the Locals key set by auth middleware
// (typically "user_id").
func Handler(hub *Hub, userIDKey string) fiber.Handler {
	return websocket.New(func(c *websocket.Conn) {
		var userID uuid.UUID

		// gofiber/websocket/v2 propagates Locals from the Fiber ctx to the
		// websocket.Conn — but only values set BEFORE websocket.New runs.
		// The auth middleware sets user_id before the upgrade, so it's available.
		switch v := c.Locals(userIDKey).(type) {
		case uuid.UUID:
			userID = v
		case string:
			if parsed, err := uuid.Parse(v); err == nil {
				userID = parsed
			}
		}

		if userID == uuid.Nil {
			log.Printf("ws: rejected — no valid %s in Locals (type: %T, value: %v)", userIDKey, c.Locals(userIDKey), c.Locals(userIDKey))
			c.WriteMessage(websocket.CloseMessage, []byte("auth required"))
			c.Close()
			return
		}

		log.Printf("ws: user %s connected", userID)
		client := &Client{Hub: hub, conn: c, send: make(chan []byte, 1024), UserID: userID}
		hub.register <- client

		go client.writePump()
		client.readPump()
	})
}

// Mount registers the WebSocket route on the given router.
// The auth middleware MUST run before the upgrade to populate Locals[userIDKey].
func Mount(router fiber.Router, hub *Hub, authMiddleware fiber.Handler, userIDKey string) {
	router.Use("/ws", func(c fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})
	router.Get("/ws", authMiddleware, Handler(hub, userIDKey))
}
