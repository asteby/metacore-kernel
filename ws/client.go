package ws

import (
	"log"
	"sync"
	"time"

	"github.com/gofiber/contrib/v3/websocket"
	"github.com/google/uuid"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
	maxMsgSize = 4096
)

// ClientOf is a single WebSocket connection bound to a user, keyed by the
// host's user-ID type.
type ClientOf[K comparable] struct {
	Hub    *HubOf[K]
	conn   *websocket.Conn
	send   chan []byte
	UserID K

	// Context holds arbitrary app-level state (e.g. active conversation ID).
	// Apps set this via SetContext; the hub reads it inside SendConditional.
	// Access is protected by mu.
	Context any
	mu      sync.RWMutex
}

// Client is the historical uuid-keyed client, kept as an alias for back-compat.
type Client = ClientOf[uuid.UUID]

// SetContext stores arbitrary per-connection state for use in SendConditional predicates.
func (c *ClientOf[K]) SetContext(ctx any) {
	c.mu.Lock()
	c.Context = ctx
	c.mu.Unlock()
}

// GetContext retrieves the stored per-connection context.
func (c *ClientOf[K]) GetContext() any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Context
}

func (c *ClientOf[K]) readPump() {
	defer func() {
		c.Hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMsgSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("ws: read error: %v", err)
			}
			break
		}
		// Inbound messages from client can be handled here if needed
	}
}

func (c *ClientOf[K]) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)
			w.Close()
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
