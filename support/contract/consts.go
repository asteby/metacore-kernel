package contract

// Callback and desk events. The first group travels Hub -> instance as the
// "event" of a callback body (X-Hub-Event). case_opened, message and
// case_resolved are Hub -> desk ingest events and never reach an instance.
const (
	EventIssueCommented = "issue_commented"
	EventIssueClosed    = "issue_closed"
	EventDeskMessage    = "desk_message"
	EventDeskResolved   = "desk_resolved"
	EventDeskReopened   = "desk_reopened"
	EventAIAction       = "ai_action"

	EventCaseOpened   = "case_opened"
	EventMessage      = "message"
	EventCaseResolved = "case_resolved"
)

// CallbackEvents are the events an instance may receive.
var CallbackEvents = []string{
	EventIssueCommented, EventIssueClosed, EventDeskMessage,
	EventDeskResolved, EventDeskReopened, EventAIAction,
}

// DeskEvents are the events the Hub sends to the desk ingest endpoint.
var DeskEvents = []string{
	EventCaseOpened, EventMessage, EventCaseResolved, EventDeskReopened,
	EventVisitorErased,
}

// IsCallbackEvent reports whether e is a callback event this contract knows.
// Receivers must still answer 2xx to unknown events (see README.md).
func IsCallbackEvent(e string) bool { return inList(CallbackEvents, e) }

// Link source and state (support_links.source / .state).
const (
	LinkSourceManual = "manual"
	LinkSourceAuto   = "auto"

	LinkStatePending = "pending"
	LinkStateActive  = "active"
)

// ValidLinkSource reports whether s is manual or auto.
func ValidLinkSource(s string) bool { return s == LinkSourceManual || s == LinkSourceAuto }

// ValidLinkState reports whether s is pending or active.
func ValidLinkState(s string) bool { return s == LinkStatePending || s == LinkStateActive }

// Severity is the closed list accepted in an escalation.
const (
	SeverityBlocker = "blocker"
	SeverityHigh    = "high"
	SeverityNormal  = "normal"
	SeverityLow     = "low"
)

// Severities in descending order of urgency.
var Severities = []string{SeverityBlocker, SeverityHigh, SeverityNormal, SeverityLow}

// NormalizeSeverity returns s (trimmed, lowercased) when it is in the closed
// list and SeverityNormal otherwise. Unknown values never reject a request,
// to tolerate old clients.
func NormalizeSeverity(s string) string {
	s = lowerTrim(s)
	if inList(Severities, s) {
		return s
	}
	return SeverityNormal
}

// Source channels declared by the client (Source.Channel).
const (
	ChannelAby          = "aby"
	ChannelAddonSupport = "addon_soporte"
	ChannelAPI          = "api"
)

// Case delivery channels (CaseRef.Channel).
const (
	CaseChannelDesk   = "desk"
	CaseChannelGitHub = "github"
	CaseChannelBoth   = "both"
)

// Transcript roles and message author types.
const (
	RoleUser  = "user"
	RoleAgent = "agent"
	RoleAI    = "ai"

	AuthorReporter = "reporter"
	AuthorAgent    = "agent"
)

// Signed request headers of an instance (unchanged since v1) and callback
// headers of the Hub.
const (
	HeaderInstancePubkey = "X-Instance-Pubkey"
	HeaderSigTimestamp   = "X-Signature-Timestamp"
	HeaderSignature      = "X-Signature"

	HeaderHubSignature = "X-Hub-Signature"
	HeaderHubTimestamp = "X-Hub-Timestamp"
	HeaderHubDelivery  = "X-Hub-Delivery"
	HeaderHubEvent     = "X-Hub-Event"
)

// Endpoint paths, relative to the Hub base URL.
const (
	PathEscalate = "/v1/support/escalate"
	PathMessage  = "/v1/support/message"
	PathLink     = "/v1/support/link"
	PathLinkAck  = "/v1/support/link/ack"
)

func inList(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
