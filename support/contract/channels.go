package contract

import "time"

// Support channels: the optional fields a channel adds to the desk case, the
// thread views shared by the widget API, the instance API and the SDK, and the
// pure projection of a callback event onto a thread. Everything here is
// additive within contract v2.

// Channel kinds (ChannelRef.Kind).
const (
	ChannelKindPlatform = "platform"
	ChannelKindOrg      = "org"
)

// Requester kinds (Requester.Kind): an OPS instance, an end customer using the
// widget, or a company backend using the server API.
const (
	RequesterInstance = "instance"
	RequesterVisitor  = "visitor"
	RequesterAPI      = "api"
)

// ChannelRef is the channel a desk case belongs to. ID is the internal channel
// id and only travels towards the desk; PublicID is the stable public one.
type ChannelRef struct {
	ID       string `json:"id"`
	PublicID string `json:"public_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// Requester is who opened the case. Verified is false for an anonymous
// visitor. UserRef is the opaque per-channel reference (same concept as
// Source.UserRef); Email is only present when the channel shares contact data.
type Requester struct {
	Kind        string `json:"kind"`
	Verified    bool   `json:"verified"`
	DisplayName string `json:"display_name"`
	UserRef     string `json:"user_ref"`
	Email       string `json:"email,omitempty"`
}

// Case status as shown in a thread.
const (
	CaseStatusOpen     = "open"
	CaseStatusResolved = "resolved"
	CaseStatusClosed   = "closed"
)

// Thread author types (MessageView.AuthorType). reporter and agent are the
// wire author types; ai and system only appear in views.
const (
	AuthorAI     = "ai"
	AuthorSystem = "system"
)

// EventVisitorErased is sent to the desk when an end customer is erased, so
// the desk deletes its own copy. It is a desk event, never a callback event.
const EventVisitorErased = "visitor_erased"

// MessageView is a message of a thread, as the UI paints it.
type MessageView struct {
	ID          string       `json:"id"`
	AuthorType  string       `json:"author_type"`
	AuthorName  string       `json:"author_name,omitempty"`
	Body        string       `json:"body"`
	CreatedAt   string       `json:"created_at"` // RFC 3339
	Attachments []Attachment `json:"attachments,omitempty"`
}

// View converts a desk message into a thread message.
func (m DeskMessage) View() MessageView {
	return MessageView{
		ID: m.ID, AuthorType: m.AuthorType, AuthorName: m.AuthorName, Body: m.Body,
		CreatedAt: m.CreatedAt.UTC().Format(time.RFC3339), Attachments: m.Attachments,
	}
}

// CaseView is a case with its latest messages, in chronological order.
// MessagesOlderCursor is absent when there are no older messages. HubState is
// only filled by an instance (pending, synced or failed). Channel is the
// public channel id.
type CaseView struct {
	CaseID              string        `json:"case_id,omitempty"`
	Folio               string        `json:"folio"`
	Channel             string        `json:"channel,omitempty"`
	Subject             string        `json:"subject"`
	Description         string        `json:"description,omitempty"`
	Status              string        `json:"status"`
	Severity            string        `json:"severity,omitempty"`
	HubState            string        `json:"hub_state,omitempty"`
	Solution            string        `json:"solution,omitempty"`
	OpenedAt            string        `json:"opened_at,omitempty"`
	UpdatedAt           string        `json:"updated_at,omitempty"`
	ResolvedAt          string        `json:"resolved_at,omitempty"`
	Messages            []MessageView `json:"messages"`
	MessagesOlderCursor string        `json:"messages_older_cursor,omitempty"`
}

// VisitorErasedEvent is the signed body the Hub POSTs to the desk ingest
// endpoint when an end customer is erased. CaseIDs lists the cases removed.
type VisitorErasedEvent struct {
	DeliveryID string     `json:"delivery_id"`
	Event      string     `json:"event"`
	Timestamp  int64      `json:"timestamp"`
	Channel    ChannelRef `json:"channel"`
	UserRef    string     `json:"user_ref"`
	CaseIDs    []string   `json:"case_ids,omitempty"`
	ErasedAt   string     `json:"erased_at"` // RFC 3339
	Contract   int        `json:"contract,omitempty"`
}

// ProjectEvent projects a callback event onto a thread: the message it adds
// and the case status it implies. The message has an empty ID when the event
// adds none; status is empty when the event leaves the status unchanged. known
// is false for an event this contract does not describe, which receivers must
// still acknowledge. EventVisitorErased is known and projects to nothing.
func ProjectEvent(e CallbackEvent) (msg MessageView, status string, known bool) {
	id := e.MessageID
	if id == "" {
		id = e.DeliveryID
	}
	base := MessageView{ID: id, AuthorName: e.AuthorName, Attachments: e.Attachments}
	if e.Timestamp > 0 {
		base.CreatedAt = time.Unix(e.Timestamp, 0).UTC().Format(time.RFC3339)
	}
	switch e.Event {
	case EventDeskMessage:
		base.AuthorType, base.Body = AuthorAgent, e.Body
		return base, "", true
	case EventIssueCommented:
		base.AuthorType, base.Body = AuthorAgent, firstNonEmpty(e.Body, e.Comment)
		return base, "", true
	case EventAIAction:
		base.AuthorType, base.Body = AuthorAI, e.Body
		return base, "", true
	case EventDeskResolved:
		base.AuthorType, base.Body = AuthorSystem, firstNonEmpty(e.Solution, e.Body)
		return base, CaseStatusResolved, true
	case EventIssueClosed:
		base.AuthorType, base.Body = AuthorSystem, firstNonEmpty(e.Solution, e.Comment)
		return base, CaseStatusClosed, true
	case EventDeskReopened:
		base.AuthorType, base.Body = AuthorSystem, e.Body
		return base, CaseStatusOpen, true
	case EventVisitorErased:
		return MessageView{}, "", true
	}
	return MessageView{}, "", false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
