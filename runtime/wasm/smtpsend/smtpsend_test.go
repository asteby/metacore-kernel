package smtpsend

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a tiny in-process SMTP server: implicit TLS or STARTTLS, AUTH PLAIN/LOGIN, and
// the usual MAIL/RCPT/DATA. It records what it received.
type fakeServer struct {
	ln       net.Listener
	cert     tls.Certificate
	implicit bool
	starttls bool
	authMech string // "PLAIN LOGIN"
	user     string
	pass     string
	rejectRC string // recipient to reject with 550

	mu       sync.Mutex
	gotMail  string
	gotRcpt  []string
	gotData  string
	gotAuth  bool
	sawPlain bool // a command was read before TLS was up (must stay false for TLS-only flows)
}

func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mail.test"}, DNSNames: []string{"mail.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func newServer(t *testing.T, implicit, starttls bool) (*fakeServer, *x509.CertPool) {
	t.Helper()
	cert, pool := selfSigned(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{ln: ln, cert: cert, implicit: implicit, starttls: starttls, authMech: "PLAIN LOGIN", user: "ventas@7leguas.test", pass: "s3cr3t-pass"}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s, pool
}

func (s *fakeServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *fakeServer) handle(c net.Conn) {
	defer c.Close()
	cfg := &tls.Config{Certificates: []tls.Certificate{s.cert}}
	tlsUp := false
	if s.implicit {
		c = tls.Server(c, cfg)
		tlsUp = true
	}
	r := bufio.NewReader(c)
	w := func(line string) { c.Write([]byte(line + "\r\n")) }
	w("220 mail.test ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		if !tlsUp && !strings.HasPrefix(up, "EHLO") && !strings.HasPrefix(up, "STARTTLS") && !strings.HasPrefix(up, "QUIT") {
			s.mu.Lock()
			s.sawPlain = true
			s.mu.Unlock()
		}
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			w("250-mail.test")
			if s.starttls && !tlsUp {
				w("250-STARTTLS")
			}
			if tlsUp {
				w("250-AUTH " + s.authMech)
			}
			w("250 SIZE 26214400")
		case up == "STARTTLS":
			w("220 ready")
			c = tls.Server(c, cfg)
			r = bufio.NewReader(c)
			w = func(l string) { c.Write([]byte(l + "\r\n")) }
			tlsUp = true
		case strings.HasPrefix(up, "AUTH PLAIN"):
			enc := strings.TrimSpace(line[len("AUTH PLAIN"):])
			raw, _ := base64.StdEncoding.DecodeString(enc)
			parts := strings.Split(string(raw), "\x00")
			if len(parts) == 3 && parts[1] == s.user && parts[2] == s.pass {
				s.mu.Lock()
				s.gotAuth = true
				s.mu.Unlock()
				w("235 ok")
			} else {
				w("535 5.7.8 authentication failed")
			}
		case up == "AUTH LOGIN":
			w("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
			u, _ := r.ReadString('\n')
			w("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
			p, _ := r.ReadString('\n')
			du, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(u))
			dp, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(p))
			if string(du) == s.user && string(dp) == s.pass {
				s.mu.Lock()
				s.gotAuth = true
				s.mu.Unlock()
				w("235 ok")
			} else {
				w("535 5.7.8 authentication failed")
			}
		case strings.HasPrefix(up, "MAIL FROM:"):
			s.mu.Lock()
			s.gotMail = strings.Trim(line[len("MAIL FROM:"):], "<> ")
			s.mu.Unlock()
			w("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			rc := strings.Trim(line[len("RCPT TO:"):], "<> ")
			if s.rejectRC != "" && rc == s.rejectRC {
				w("550 5.1.1 no such user")
				continue
			}
			s.mu.Lock()
			s.gotRcpt = append(s.gotRcpt, rc)
			s.mu.Unlock()
			w("250 ok")
		case up == "DATA":
			w("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.gotData = b.String()
			s.mu.Unlock()
			w("250 queued")
		case up == "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

// point makes the package dial the fake server (the real dialer refuses loopback) and trust its cert.
func point(t *testing.T, s *fakeServer, pool *x509.CertPool) {
	t.Helper()
	oldD, oldT := DialContext, TLSConfig
	DialContext = func(ctx context.Context, network, _ string, timeout time.Duration) (net.Conn, error) {
		return (&net.Dialer{Timeout: timeout}).DialContext(ctx, network, s.ln.Addr().String())
	}
	TLSConfig = func(host string) *tls.Config { return &tls.Config{ServerName: host, RootCAs: pool, MinVersion: tls.VersionTLS12} }
	t.Cleanup(func() { DialContext, TLSConfig = oldD, oldT })
}

func req(security string) *Request {
	return &Request{
		Host: "mail.test", Port: 587, Security: security,
		Username: "ventas@7leguas.test", Password: "s3cr3t-pass", Helo: "ops.test",
		From: "ventas@7leguas.test", To: []string{"cliente@example.com"},
		MessageBase64: base64.StdEncoding.EncodeToString([]byte("From: ventas@7leguas.test\r\nTo: cliente@example.com\r\nSubject: hola\r\n\r\ncuerpo\r\n")),
	}
}

func send(t *testing.T, r *Request) Result {
	t.Helper()
	msg, bad := Validate(r)
	if bad != "" {
		t.Fatalf("validate: %s", bad)
	}
	return Deliver(context.Background(), r, msg)
}

func TestSTARTTLSDeliversWithAuthPlain(t *testing.T) {
	s, pool := newServer(t, false, true)
	point(t, s, pool)
	res := send(t, req("starttls"))
	if res.Code != "" {
		t.Fatalf("esperaba éxito, obtuve %+v", res)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.gotAuth || s.gotMail != "ventas@7leguas.test" || len(s.gotRcpt) != 1 || !strings.Contains(s.gotData, "Subject: hola") {
		t.Fatalf("el servidor no recibió lo esperado: auth=%v mail=%q rcpt=%v data=%q", s.gotAuth, s.gotMail, s.gotRcpt, s.gotData)
	}
	if s.sawPlain {
		t.Fatal("un comando (auth, mail…) viajó antes de TLS")
	}
}

func TestImplicitTLSDelivers(t *testing.T) {
	s, pool := newServer(t, true, false)
	point(t, s, pool)
	r := req("tls")
	r.Port = 465
	if res := send(t, r); res.Code != "" {
		t.Fatalf("esperaba éxito, obtuve %+v", res)
	}
}

func TestAuthLoginWhenPlainNotOffered(t *testing.T) {
	s, pool := newServer(t, true, false)
	s.authMech = "LOGIN"
	point(t, s, pool)
	r := req("tls")
	r.Port = 465
	if res := send(t, r); res.Code != "" {
		t.Fatalf("AUTH LOGIN falló: %+v", res)
	}
}

func TestWrongPasswordIsSmtpAuthAndNeverEchoed(t *testing.T) {
	s, pool := newServer(t, false, true)
	point(t, s, pool)
	r := req("starttls")
	r.Password = "equivocada-123"
	res := send(t, r)
	if res.Code != "smtp_auth" || res.SMTPCode != 535 {
		t.Fatalf("esperaba smtp_auth 535, obtuve %+v", res)
	}
	if strings.Contains(res.Message, "equivocada-123") || strings.Contains(res.Message, "s3cr3t-pass") {
		t.Fatalf("el resultado filtra la contraseña: %q", res.Message)
	}
}

func TestStartTLSRefusedWhenServerDoesNotOfferIt(t *testing.T) {
	s, pool := newServer(t, false, false) // sin STARTTLS: jamás se manda en claro
	point(t, s, pool)
	res := send(t, req("starttls"))
	if res.Code != "smtp_tls" {
		t.Fatalf("esperaba smtp_tls, obtuve %+v", res)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gotAuth || s.sawPlain {
		t.Fatal("no debió autenticar ni mandar comandos sin TLS")
	}
}

func TestRecipientRejectedIsSmtpRejected(t *testing.T) {
	s, pool := newServer(t, false, true)
	s.rejectRC = "cliente@example.com"
	point(t, s, pool)
	res := send(t, req("starttls"))
	if res.Code != "smtp_rejected" || res.SMTPCode != 550 {
		t.Fatalf("esperaba smtp_rejected 550, obtuve %+v", res)
	}
}

func TestVerifyOnlyDoesNotSend(t *testing.T) {
	s, pool := newServer(t, false, true)
	point(t, s, pool)
	r := &Request{Host: "mail.test", Port: 587, Security: "starttls", Username: "ventas@7leguas.test", Password: "s3cr3t-pass", VerifyOnly: true}
	if _, bad := Validate(r); bad != "" {
		t.Fatal(bad)
	}
	if res := Deliver(context.Background(), r, nil); res.Code != "" {
		t.Fatalf("verify falló: %+v", res)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.gotAuth || s.gotMail != "" || s.gotData != "" {
		t.Fatal("verify_only debe autenticar y no mandar nada")
	}
}

func TestValidateRefusesUnsafeRequests(t *testing.T) {
	cases := map[string]func(r *Request){
		"puerto fuera de lista":   func(r *Request) { r.Port = 22 },
		"sin TLS":                 func(r *Request) { r.Security = "none" },
		"host con salto de línea": func(r *Request) { r.Host = "mail.test\r\nRCPT TO:<x@y>" },
		"from inyectado":          func(r *Request) { r.From = "a@b.c\r\nRCPT TO:<x@y>" },
		"destinatario inyectado":  func(r *Request) { r.To = []string{"a@b.c\nBcc: x@y"} },
		"sin destinatarios":       func(r *Request) { r.To = nil },
		"demasiados":              func(r *Request) { r.To = make([]string, MaxRecipients+1); for i := range r.To { r.To[i] = "a@b.c" } },
		"mensaje vacío":           func(r *Request) { r.MessageBase64 = "" },
		"mensaje no base64":       func(r *Request) { r.MessageBase64 = "%%%" },
	}
	for name, mut := range cases {
		r := req("starttls")
		mut(r)
		if _, bad := Validate(r); bad == "" {
			t.Errorf("%s: debió rechazarse", name)
		}
	}
}

func TestTimeoutIsCapped(t *testing.T) {
	r := &Request{TimeoutMs: 10 * 60 * 1000}
	if r.Timeout() != MaxTimeout {
		t.Fatalf("timeout sin tope: %v", r.Timeout())
	}
	if (&Request{}).Timeout() != DefaultTimeout {
		t.Fatal("timeout por omisión incorrecto")
	}
}
