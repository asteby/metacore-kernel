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
//
// The hub is generic over the user-ID key (HubOf[K comparable]) so hosts
// with legacy numeric or string IDs can adopt it without migrating to UUIDs
// (upstream-first: contributed while onboarding doctores.lat). `Hub` and
// `Client` remain aliases of the uuid.UUID instantiation for back-compat.
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

	// WebRTC / realtime call signaling (hosts relay these over the hub).
	// Scalable mesh or SFU clients share this contract; the hub stays
	// transport-only and does not interpret SDP/ICE payloads.
	MsgCallInvite MessageType = "CALL_INVITE"
	MsgCallJoin   MessageType = "CALL_JOIN"
	MsgCallSignal MessageType = "CALL_SIGNAL"
	MsgCallEnd    MessageType = "CALL_END"
)

// Message is the envelope sent over the wire.
type Message struct {
	Type    MessageType `json:"type"`
	Payload any         `json:"payload"`
}

// HubOf maintains connected clients and routes messages, keyed by the
// host's user-ID type (uuid.UUID, int64, string, ...).
type HubOf[K comparable] struct {
	clients     map[K]map[*ClientOf[K]]bool
	register    chan *ClientOf[K]
	unregister  chan *ClientOf[K]
	broadcast   chan *broadcastMsg[K]
	batchCast   chan *batchMsg[K]
	conditional chan *conditionalMsg[K]
	mu          sync.RWMutex

	// OnNotification is called when a NOTIFICATION message is sent to a user.
	// Apps use this to persist notifications to DB. Optional.
	OnNotification func(userID K, msg Message)
}

// Hub is the historical uuid-keyed hub. Kept as an alias so existing hosts
// (link, ops) compile unchanged.
type Hub = HubOf[uuid.UUID]

type broadcastMsg[K comparable] struct {
	UserID  K
	Message Message
}

type batchMsg[K comparable] struct {
	UserIDs []K
	Message Message
}

// conditionalMsg routes different messages to a user based on a per-client predicate.
// This is the generic equivalent of a "smart broadcast" (conversation-aware routing).
type conditionalMsg[K comparable] struct {
	UserID    K
	Predicate func(clientCtx any) bool // called with Client.Context; true → primary
	Primary   Message                  // sent when predicate returns true
	Fallback  Message                  // sent otherwise
}

// NewHubOf creates a hub keyed by K. Call Run() in a goroutine before
// accepting connections.
func NewHubOf[K comparable]() *HubOf[K] {
	return &HubOf[K]{
		clients:     make(map[K]map[*ClientOf[K]]bool),
		register:    make(chan *ClientOf[K]),
		unregister:  make(chan *ClientOf[K]),
		broadcast:   make(chan *broadcastMsg[K], 256),
		batchCast:   make(chan *batchMsg[K], 64),
		conditional: make(chan *conditionalMsg[K], 64),
	}
}

// NewHub creates the historical uuid-keyed Hub.
func NewHub() *Hub {
	return NewHubOf[uuid.UUID]()
}

// Run starts the hub event loop. Blocks forever — run in a goroutine.
func (h *HubOf[K]) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			if _, ok := h.clients[client.UserID]; !ok {
				h.clients[client.UserID] = make(map[*ClientOf[K]]bool)
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
				targets := make([]*ClientOf[K], 0, len(clients))
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
func (h *HubOf[K]) SendToUser(userID K, msg Message) {
	h.broadcast <- &broadcastMsg[K]{UserID: userID, Message: msg}
}

// SendToUsers sends a message to a list of users.
func (h *HubOf[K]) SendToUsers(userIDs []K, msg Message) {
	h.batchCast <- &batchMsg[K]{UserIDs: userIDs, Message: msg}
}

// SendConditional delivers different messages to a user's connections based on
// a per-connection predicate. This is the generic equivalent of a
// "smart broadcast" (conversation-aware routing).
//
// Each active connection for userID has its Context examined; if predicate
// returns true the primary message is sent, otherwise the fallback.
// Context is set by the app via Client.SetContext before or after registration.
func (h *HubOf[K]) SendConditional(userID K, predicate func(ctx any) bool, primary, fallback Message) {
	h.conditional <- &conditionalMsg[K]{
		UserID:    userID,
		Predicate: predicate,
		Primary:   primary,
		Fallback:  fallback,
	}
}

// ConnectedUsers returns a snapshot of currently connected user IDs.
func (h *HubOf[K]) ConnectedUsers() []K {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]K, 0, len(h.clients))
	for uid := range h.clients {
		out = append(out, uid)
	}
	return out
}

func (h *HubOf[K]) sendToUser(userID K, msg Message) {
	data, _ := json.Marshal(msg)
	h.mu.RLock()
	clients, ok := h.clients[userID]
	if !ok {
		h.mu.RUnlock()
		return
	}
	targets := make([]*ClientOf[K], 0, len(clients))
	for c := range clients {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		sendBytes(h, c, data)
	}
}

func sendBytes[K comparable](h *HubOf[K], c *ClientOf[K], data []byte) {
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
