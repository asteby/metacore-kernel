package contract

import "time"

// Wire types. Every field added after v1 is optional (omitempty), so a v1
// body decodes into these types and a v2 body is a superset. Decoders must
// not use DisallowUnknownFields in production: receivers ignore unknown
// fields (see README.md).

// Ticket is the ticket of the instance being escalated.
type Ticket struct {
	Folio       string `json:"folio"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Priority    string `json:"priority,omitempty"`
}

// Turn is one transcript entry. At is v2 and optional.
type Turn struct {
	Role string `json:"role"`
	Text string `json:"text"`
	At   string `json:"at,omitempty"` // RFC 3339
}

// Source says where the escalation was raised (v2).
type Source struct {
	Channel string `json:"channel,omitempty"`
	// UserRef is an opaque HMAC of the user id with an instance-specific
	// salt, never an email. The desk uses it to group cases only.
	UserRef string `json:"user_ref,omitempty"`
	Locale  string `json:"locale,omitempty"`
}

// CallbackRef is the callback the instance declares (v1 and v2).
type CallbackRef struct {
	URL string `json:"url"`
}

// AttachmentRef is an attachment of an escalation by reference (v2).
type AttachmentRef struct {
	Ref  string `json:"ref"`
	Name string `json:"name,omitempty"`
	Mime string `json:"mime,omitempty"`
	Size int64  `json:"size,omitempty"`
}

// EscalateRequest is the body of POST /v1/support/escalate. A v1 client sends
// Ticket, Summary, Transcript and Callback only.
type EscalateRequest struct {
	Contract    int             `json:"contract,omitempty"`
	Ticket      Ticket          `json:"ticket"`
	Severity    string          `json:"severity,omitempty"`
	Summary     string          `json:"summary,omitempty"`
	Transcript  []Turn          `json:"transcript,omitempty"`
	Source      *Source         `json:"source,omitempty"`
	Callback    CallbackRef     `json:"callback"`
	Attachments []AttachmentRef `json:"attachments,omitempty"`
}

// CaseRef identifies the case the Hub opened (v2 response).
type CaseRef struct {
	ID      string `json:"id"`
	Folio   string `json:"folio"`
	Channel string `json:"channel"`
}

// LinkRef is the link summary of an escalate response (v2).
type LinkRef struct {
	Source string `json:"source"`
	State  string `json:"state"`
}

// EscalateResponse is the 201 (or duplicate 200) body of /escalate. The v1
// fields IssueURL, IssueNumber and Status are always present; Case, Link and
// CallbackSecret are v2. CallbackSecret is only returned while Link.State is
// pending and Link.Source is auto.
type EscalateResponse struct {
	IssueURL       string   `json:"issue_url"`
	IssueNumber    int      `json:"issue_number"`
	Status         string   `json:"status"`
	Duplicate      bool     `json:"duplicate,omitempty"`
	Case           *CaseRef `json:"case,omitempty"`
	Link           *LinkRef `json:"link,omitempty"`
	CallbackSecret string   `json:"callback_secret,omitempty"`
}

// Attachment is a message attachment: a short-lived signed URL given by
// whoever attaches it; the Hub never carries the file itself. MimeType is an
// accepted alias of ContentType on input.
type Attachment struct {
	URL         string `json:"url"`
	Name        string `json:"name,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	MimeType    string `json:"mime_type,omitempty"`
	Size        int64  `json:"size,omitempty"`
}

// MessageAuthor is the author of a /message request.
type MessageAuthor struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// MessageRequest is the body of POST /v1/support/message. MessageID is the v1
// idempotency key; ClientMessageID is its v2 name, which a v2 client must
// send.
type MessageRequest struct {
	Ticket struct {
		Folio string `json:"folio"`
	} `json:"ticket"`
	Author          MessageAuthor `json:"author"`
	Body            string        `json:"body"`
	MessageID       string        `json:"message_id,omitempty"`
	ClientMessageID string        `json:"client_message_id,omitempty"`
	Attachments     []Attachment  `json:"attachments,omitempty"`
}

// ID returns the idempotency key of the message: ClientMessageID when set,
// MessageID otherwise.
func (m MessageRequest) ID() string {
	if m.ClientMessageID != "" {
		return m.ClientMessageID
	}
	return m.MessageID
}

// MessageResponse is the 202 body of /message.
type MessageResponse struct {
	Status    string `json:"status"`
	MessageID string `json:"message_id"`
	Duplicate bool   `json:"duplicate"`
}

// LinkInfo is the link state of a /link response.
type LinkInfo struct {
	Source      string `json:"source"`
	State       string `json:"state"`
	Enabled     bool   `json:"enabled"`
	CallbackURL string `json:"callback_url,omitempty"`
}

// LinkRequest is the optional body of POST /v1/support/link.
type LinkRequest struct {
	Contract int         `json:"contract,omitempty"`
	Callback CallbackRef `json:"callback"`
}

// LinkResponse is the body of POST /v1/support/link. CallbackSecret is
// returned only while the link is pending.
type LinkResponse struct {
	Link           LinkInfo `json:"link"`
	CallbackSecret string   `json:"callback_secret,omitempty"`
}

// LinkAckRequest is the body of POST /v1/support/link/ack. Proof is
// AckProof(secret, instanceID, ts) where ts is the X-Signature-Timestamp of
// the same request.
type LinkAckRequest struct {
	Proof string `json:"proof"`
}

// CallbackEvent is the signed body the Hub POSTs to an instance. The fields
// of v1 are Event, TicketFolio, IssueURL, IssueNumber, Comment, Solution,
// DeliveryID and Timestamp; the desk events add Body, AuthorName, MessageID
// and Attachments; v2 adds Contract.
type CallbackEvent struct {
	Event       string       `json:"event"`
	TicketFolio string       `json:"ticket_folio"`
	IssueURL    string       `json:"issue_url"`
	IssueNumber int          `json:"issue_number"`
	Comment     string       `json:"comment"`
	Solution    string       `json:"solution"`
	Body        string       `json:"body,omitempty"`
	AuthorName  string       `json:"author_name,omitempty"`
	MessageID   string       `json:"message_id,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	// Action names what the AI did, only for ai_action (v2).
	Action     string `json:"action,omitempty"`
	DeliveryID string `json:"delivery_id"`
	Timestamp  int64  `json:"timestamp"`
	Contract   int    `json:"contract,omitempty"`
}

// IncidentRef is a recent, already redacted fleet event in an InstanceContext.
type IncidentRef struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	At      string `json:"at"`
}

// InstanceContext is what the Hub knows about the instance that escalated.
// It is added by the Hub, travels in case_opened towards the desk and is never
// returned to the instance.
type InstanceContext struct {
	InstanceID      string        `json:"instance_id"`
	Name            string        `json:"name,omitempty"`
	Product         string        `json:"product,omitempty"`
	Version         string        `json:"version,omitempty"`
	Customer        string        `json:"customer,omitempty"`
	LicenseStatus   string        `json:"license_status,omitempty"`
	Partner         string        `json:"partner,omitempty"`
	Severity        string        `json:"severity,omitempty"`
	RecentIncidents []IncidentRef `json:"recent_incidents,omitempty"`
	// Plan is the commercial plan label of the license; LastHeartbeat is the
	// RFC 3339 time of the last heartbeat of the instance. Both optional.
	Plan          string `json:"plan,omitempty"`
	LastHeartbeat string `json:"last_heartbeat,omitempty"`
}

// DeskCase is the case as the desk sees it.
type DeskCase struct {
	InstanceID      string           `json:"instance_id"`
	InstanceName    string           `json:"instance_name"`
	CompanyName     string           `json:"company_name"`
	Folio           string           `json:"folio"`
	Subject         string           `json:"subject"`
	Description     string           `json:"description"`
	Priority        string           `json:"priority"`
	IssueURL        string           `json:"issue_url"`
	IssueNumber     int              `json:"issue_number"`
	OpenedAt        time.Time        `json:"opened_at"`
	Severity        string           `json:"severity,omitempty"`
	InstanceContext *InstanceContext `json:"instance_context,omitempty"`
	// CaseID, Channel and Requester are the channel fields (see channels.go).
	// They are optional: a desk that does not know them ignores them.
	CaseID    string      `json:"case_id,omitempty"`
	Channel   *ChannelRef `json:"channel,omitempty"`
	Requester *Requester  `json:"requester,omitempty"`
}

// DeskMessage is a message of a desk case.
type DeskMessage struct {
	ID          string       `json:"id"`
	AuthorType  string       `json:"author_type"`
	AuthorName  string       `json:"author_name"`
	Body        string       `json:"body"`
	CreatedAt   time.Time    `json:"created_at"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// DeskIngestEvent is the signed body the Hub POSTs to the desk ingest
// endpoint (case_opened, message, case_resolved, desk_reopened).
type DeskIngestEvent struct {
	DeliveryID string       `json:"delivery_id"`
	Event      string       `json:"event"`
	Timestamp  int64        `json:"timestamp"`
	Case       DeskCase     `json:"case"`
	Message    *DeskMessage `json:"message,omitempty"`
	Solution   string       `json:"solution,omitempty"`
	Contract   int          `json:"contract,omitempty"`
}
