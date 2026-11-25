package contract

import "time"

// EmbedSessionPurpose is the signedenvelope purpose of the widget session.
// The Hub seals EmbedSessionClaims under it; the domain separation keeps a
// session from verifying as any other signed envelope.
const EmbedSessionPurpose = "support.embed.session"

// IdentityAudience is the "aud" claim of the identity token a company backend
// signs for an identified visitor.
const IdentityAudience = "support.embed"

// EmbedSessionClaims is the payload of the widget session envelope. Channel
// and Visitor are internal uuids, Origin is the exact parent origin the
// session was opened for, Identified tells whether the visitor presented a
// valid company identity. NotBefore and Expires are unix seconds.
type EmbedSessionClaims struct {
	Channel    string `json:"ch"`
	Visitor    string `json:"vis"`
	Origin     string `json:"ori"`
	Identified bool   `json:"idn"`
	NotBefore  int64  `json:"nbf"`
	Expires    int64  `json:"exp"`
	JTI        string `json:"jti"`
}

// ActiveAt reports whether the session window contains now.
func (c EmbedSessionClaims) ActiveAt(now time.Time) bool {
	t := now.Unix()
	return c.Expires > 0 && t >= c.NotBefore && t < c.Expires
}
