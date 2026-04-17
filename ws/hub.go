// Package ws provides a generic WebSocket hub for real-time communication.
//
// MessageType is a plain string so each consuming app can declare its own
// constants without forking the package:
//
//	const (
//	    MsgNewMessage ws.MessageType = "NEW_MESSAGE"
//	    MsgTicket     ws.MessageType = "TICKET_UPDATE"
//	)
//
// Routing: clients connect per user; the hub delivers to every open connection
// for that user.  For org-wide broadcast, callers query their own DB for user
// IDs and call SendToUsers.  Notification persistence is delegated to the
// optional OnNotification hook so the hub stays ORM-free.
package ws

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/google/uuid"
)

// MessageType categorizes WebSocket messages.
type MessageType string

const (
	MsgNotification MessageType = "NOTIFICATION"
	MsgStatusUpdate MessageType = "STATUS_UPDATE"
	MsgCustom       MessageType = "CUSTOM"
)

// Message is the envelope sent over the wire.
type Message struct {
	Type    MessageType `json:"type"`
	Payload any         `json:"payload"`
}

// Hub maintains connected clients and routes messages.
type Hub struct {
	clients    map[uuid.UUID]map[*Client]bool
	register   chan *Client
	unregister chan *Client
	broadcast  chan *broadcastMsg
	batchCast  chan *batchMsg
	conditional chan *conditionalMsg
	mu         sync.RWMutex

	// OnNotification is called when a NOTIFICATION message is sent to a user.
	// Apps use this to persist notifications to DB. Optional.
	OnNotification func(userID uuid.UUID, msg Message)
}

type broadcastMsg struct {
	UserID  uuid.UUID
	Message Message
}

type batchMsg struct {
	UserIDs []uuid.UUID
	Message Message
}

// conditionalMsg routes different messages to a user based on a per-client predicate.
// This is the generic equivalent of link's "smart broadcast" (conversation-aware routing).
type conditionalMsg struct {
	UserID    uuid.UUID
	Predicate func(clientCtx any) bool // called with Client.Context; true → primary
	Primary   Message                  // sent when predicate returns true
	Fallback  Message                  // sent otherwise
}

// NewHub creates a Hub. Call Run() in a goroutine before accepting connections.
func NewHub() *Hub {
	return &Hub{
		clients:     make(map[uuid.UUID]map[*Client]bool),
		register:    make(chan *Client),
		unregister:  make(chan *Client),
		broadcast:   make(chan *broadcastMsg, 256),
		batchCast:   make(chan *batchMsg, 64),
		conditional: make(chan *conditionalMsg, 64),
	}
}

// Run starts the hub event loop. Blocks forever — run in a goroutine.
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			if _, ok := h.clients[client.UserID]; !ok {
				h.clients[client.UserID] = make(map[*Client]bool)
			}
			h.clients[client.UserID][client] = true
			h.mu.Unlock()

		case client := <-h.unregister:
			h.mu.Lock()
			if userClients, ok := h.clients[client.UserID]; ok {
				if _, ok := userClients[client]; ok {
					delete(userClients, client)
					close(client.send)
					if len(userClients) == 0 {
						delete(h.clients, client.UserID)
					}
				}
			}
			h.mu.Unlock()

		case msg := <-h.broadcast:
			if msg.Message.Type == MsgNotification && h.OnNotification != nil {
				go h.OnNotification(msg.UserID, msg.Message)
			}
			h.sendToUser(msg.UserID, msg.Message)

		case msg := <-h.batchCast:
			data, _ := json.Marshal(msg.Message)
			h.mu.RLock()
			for _, uid := range msg.UserIDs {
				if clients, ok := h.clients[uid]; ok {
					for c := range clients {
						sendBytes(h, c, data)
					}
				}
			}
			h.mu.RUnlock()

		case msg := <-h.conditional:
			primaryData, _ := json.Marshal(msg.Primary)
			fallbackData, _ := json.Marshal(msg.Fallback)
			h.mu.RLock()
			clients, ok := h.clients[msg.UserID]
			if ok {
				targets := make([]*Client, 0, len(clients))
				for c := range clients {
					targets = append(targets, c)
				}
				h.mu.RUnlock()
				for _, c := range targets {
					if msg.Predicate != nil && msg.Predicate(c.GetContext()) {
						sendBytes(h, c, primaryData)
					} else {
						sendBytes(h, c, fallbackData)
					}
				}
			} else {
				h.mu.RUnlock()
			}
		}
	}
}

// SendToUser sends a message to every connection of a specific user.
func (h *Hub) SendToUser(userID uuid.UUID, msg Message) {
	h.broadcast <- &broadcastMsg{UserID: userID, Message: msg}
}

// SendToUsers sends a message to a list of users.
func (h *Hub) SendToUsers(userIDs []uuid.UUID, msg Message) {
	h.batchCast <- &batchMsg{UserIDs: userIDs, Message: msg}
}

// SendConditional delivers different messages to a user's connections based on
// a per-connection predicate.  This is the generic equivalent of link's
// "smart broadcast" (conversation-aware routing).
//
// Each active connection for userID has its Context examined; if predicate
// returns true the primary message is sent, otherwise the fallback.
// Context is set by the app via Client.SetContext before or after registration.
func (h *Hub) SendConditional(userID uuid.UUID, predicate func(ctx any) bool, primary, fallback Message) {
	h.conditional <- &conditionalMsg{
		UserID:    userID,
		Predicate: predicate,
		Primary:   primary,
		Fallback:  fallback,
	}
}

// ConnectedUsers returns a snapshot of currently connected user IDs.
func (h *Hub) ConnectedUsers() []uuid.UUID {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]uuid.UUID, 0, len(h.clients))
	for uid := range h.clients {
		out = append(out, uid)
	}
	return out
}

func (h *Hub) sendToUser(userID uuid.UUID, msg Message) {
	data, _ := json.Marshal(msg)
	h.mu.RLock()
	clients, ok := h.clients[userID]
	if !ok {
		h.mu.RUnlock()
		return
	}
	targets := make([]*Client, 0, len(clients))
	for c := range clients {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		sendBytes(h, c, data)
	}
}

func sendBytes(h *Hub, c *Client, data []byte) {
	select {
	case c.send <- data:
	default:
		close(c.send)
		h.unregister <- c
	}
}

func init() {
	// Ensure log prefix for ws messages
	_ = log.Prefix()
}
