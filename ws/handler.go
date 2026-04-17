package ws

import (
	"log"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/websocket/v2"
	"github.com/google/uuid"
)

// UpgradeMiddleware checks if the request is a WebSocket upgrade.
// Mount this BEFORE the Handler on the same route.
func UpgradeMiddleware(c *fiber.Ctx) error {
	if websocket.IsWebSocketUpgrade(c) {
		c.Locals("allowed", true)
		return c.Next()
	}
	return fiber.ErrUpgradeRequired
}

// Handler returns a Fiber handler that upgrades to WebSocket and registers
// the client with the hub. userIDKey is the Locals key set by auth middleware
// (typically "user_id").
func Handler(hub *Hub, userIDKey string) fiber.Handler {
	return websocket.New(func(c *websocket.Conn) {
		var userID uuid.UUID

		switch v := c.Locals(userIDKey).(type) {
		case uuid.UUID:
			userID = v
		case string:
			if parsed, err := uuid.Parse(v); err == nil {
				userID = parsed
			}
		}

		if userID == uuid.Nil {
			log.Println("ws: rejected — no valid user_id")
			c.WriteMessage(websocket.CloseMessage, []byte("auth required"))
			c.Close()
			return
		}

		client := &Client{Hub: hub, conn: c, send: make(chan []byte, 1024), UserID: userID}
		hub.register <- client

		go client.writePump()
		client.readPump()
	})
}

// Mount registers the WebSocket route on the given router. authMiddleware
// should extract the user_id and place it in Locals[userIDKey].
func Mount(router fiber.Router, hub *Hub, authMiddleware fiber.Handler, userIDKey string) {
	router.Get("/ws", authMiddleware, UpgradeMiddleware, Handler(hub, userIDKey))
}
