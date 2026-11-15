// Package smtpsend is the network half of the smtp_send host import (ABI 1.20): one SMTP
// session with mandatory TLS, submission ports only, bounded size/recipients/time. It knows
// nothing about wazero or capabilities; runtime/wasm/smtp.go gates it (smtp:send + SSRF) and
// maps its Result to the guest envelope. It never logs or returns the password.
package smtpsend

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/netguard"
)

const (
	MaxMessageBytes = 25 << 20 // 25 MiB decoded
	MaxRecipients   = 50
	DefaultTimeout  = 30 * time.Second
	MaxTimeout      = 60 * time.Second
)

// AllowedPorts is the SMTP submission set (25 relay, 465 implicit TLS, 587 submission, 2525 alternate).
var AllowedPorts = map[int]bool{25: true, 465: true, 587: true, 2525: true}

// DialContext and TLSConfig are vars so tests can reach a local server (the real dialer refuses
// loopback) and trust its certificate.
var (
	DialContext = func(ctx context.Context, network, addr string, timeout time.Duration) (net.Conn, error) {
		d := &net.Dialer{Timeout: timeout, Control: netguard.DialControl}
		return d.DialContext(ctx, network, addr)
	}
	TLSConfig = func(host string) *tls.Config {
		return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	}
)

// Request is the JSON the guest sends to smtp_send.
type Request struct {
	Host          string   `json:"host"`
	Port          int      `json:"port"`
	Security      string   `json:"security"` // "tls" | "starttls"
	Username      string   `json:"username"`
	Password      string   `json:"password"`
	Helo          string   `json:"helo"`
	From          string   `json:"from"`
	To            []string `json:"to"`
	MessageBase64 string   `json:"message_base64"`
	VerifyOnly    bool     `json:"verify_only"`
	TimeoutMs     int      `json:"timeout_ms"`
}

// Result is the outcome: Code is "" on success, else a stable code (transport, smtp_tls,
// smtp_auth, smtp_rejected); SMTPCode is the server reply code when there is one.
type Result struct {
	Code     string
	Message  string
	SMTPCode int
}

func hasCRLF(v string) bool { return strings.ContainsAny(v, "\r\n\x00") }

// Validate checks everything that needs no network and returns the decoded message. A
// non-empty second value is the bad_request reason.
func Validate(r *Request) ([]byte, string) {
	r.Host = strings.ToLower(strings.TrimSpace(r.Host))
	r.Security = strings.ToLower(strings.TrimSpace(r.Security))
	if r.Host == "" {
		return nil, "host is required"
	}
	if !AllowedPorts[r.Port] {
		return nil, fmt.Sprintf("port %d is not allowed (use 465, 587, 2525 or 25)", r.Port)
	}
	if r.Security != "tls" && r.Security != "starttls" {
		return nil, `security must be "tls" (implicit TLS) or "starttls": plaintext SMTP is not allowed`
	}
	for _, v := range []string{r.Host, r.Username, r.Helo, r.From} {
		if hasCRLF(v) {
			return nil, "control characters are not allowed in host, username, helo or from"
		}
	}
	if r.VerifyOnly {
		return nil, ""
	}
	if strings.TrimSpace(r.From) == "" {
		return nil, "from (envelope sender) is required"
	}
	if len(r.To) == 0 {
		return nil, "at least one recipient is required"
	}
	if len(r.To) > MaxRecipients {
		return nil, fmt.Sprintf("too many recipients (max %d)", MaxRecipients)
	}
	for _, t := range r.To {
		if strings.TrimSpace(t) == "" || hasCRLF(t) {
			return nil, "invalid recipient"
		}
	}
	msg, err := base64.StdEncoding.DecodeString(r.MessageBase64)
	if err != nil || len(msg) == 0 {
		return nil, "message_base64 must be a non-empty base64 RFC 5322 message"
	}
	if len(msg) > MaxMessageBytes {
		return nil, fmt.Sprintf("message too large (max %d MiB)", MaxMessageBytes>>20)
	}
	return msg, ""
}

// Timeout returns the effective session timeout.
func (r *Request) Timeout() time.Duration {
	t := DefaultTimeout
	if r.TimeoutMs > 0 {
		t = time.Duration(r.TimeoutMs) * time.Millisecond
	}
	if t > MaxTimeout {
		t = MaxTimeout
	}
	return t
}

// loginAuth is AUTH LOGIN (Office 365 and many corporate servers offer it besides PLAIN). The
// session is always TLS here, so the credentials never travel in clear.
type loginAuth struct{ user, pass string }

func (a loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }
func (a loginAuth) Next(from []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(from))) {
	case "username:", "user name:":
		return []byte(a.user), nil
	case "password:":
		return []byte(a.pass), nil
	}
	return nil, errors.New("unexpected AUTH LOGIN challenge")
}

func fail(code string, err error) Result {
	r := Result{Code: code, Message: err.Error()}
	var te *textproto.Error
	if errors.As(err, &te) {
		r.SMTPCode = te.Code
		r.Message = fmt.Sprintf("%d %s", te.Code, te.Msg)
	}
	return r
}

func failMsg(code, msg string) Result { return Result{Code: code, Message: msg} }

// Deliver runs one SMTP session (or only connect + TLS + AUTH when r.VerifyOnly). msg is the
// decoded message from Validate.
func Deliver(ctx context.Context, r *Request, msg []byte) Result {
	timeout := r.Timeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr := net.JoinHostPort(r.Host, fmt.Sprint(r.Port))
	conn, err := DialContext(ctx, "tcp", addr, timeout)
	if err != nil {
		return failMsg("transport", "cannot connect to "+addr+": "+err.Error())
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if r.Security == "tls" {
		tc := tls.Client(conn, TLSConfig(r.Host))
		if err := tc.HandshakeContext(ctx); err != nil {
			return failMsg("smtp_tls", "TLS handshake failed: "+err.Error())
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, r.Host)
	if err != nil {
		return fail("transport", err)
	}
	defer c.Close()
	helo := strings.TrimSpace(r.Helo)
	if helo == "" {
		helo = "localhost"
	}
	if err := c.Hello(helo); err != nil {
		return fail("transport", err)
	}
	if r.Security == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return failMsg("smtp_tls", "the server does not offer STARTTLS; plaintext SMTP is not allowed")
		}
		if err := c.StartTLS(TLSConfig(r.Host)); err != nil {
			return failMsg("smtp_tls", "STARTTLS failed: "+err.Error())
		}
	}
	if r.Username != "" || r.Password != "" {
		ok, mechs := c.Extension("AUTH")
		if !ok {
			return failMsg("smtp_auth", "the server does not offer authentication")
		}
		var auth smtp.Auth
		up := strings.ToUpper(mechs)
		switch {
		case strings.Contains(up, "PLAIN"):
			auth = smtp.PlainAuth("", r.Username, r.Password, r.Host)
		case strings.Contains(up, "LOGIN"):
			auth = loginAuth{r.Username, r.Password}
		default:
			return failMsg("smtp_auth", "no supported AUTH mechanism (need PLAIN or LOGIN), server offers: "+mechs)
		}
		if err := c.Auth(auth); err != nil {
			return fail("smtp_auth", err)
		}
	}
	if r.VerifyOnly {
		_ = c.Quit()
		return Result{Message: "connected and authenticated"}
	}
	if err := c.Mail(r.From); err != nil {
		return fail("smtp_rejected", err)
	}
	for _, t := range r.To {
		if err := c.Rcpt(t); err != nil {
			return fail("smtp_rejected", err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fail("smtp_rejected", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fail("transport", err)
	}
	if err := w.Close(); err != nil {
		return fail("smtp_rejected", err)
	}
	_ = c.Quit()
	return Result{Message: "accepted by the server"}
}
