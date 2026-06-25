package routes

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/smtp"
	"net/textproto"
	"time"
)

// Attachment carries a file's metadata and raw bytes so it can be encoded
// into a MIME body (SMTP) or JSON (HTTP API).
type Attachment struct {
	FileName string
	Content  []byte // raw bytes (not base64-encoded)
}

// EmailProvider is the interface every route driver must satisfy.
type EmailProvider interface {
	Send(to string, subject string, bodyHTML string, attachments []Attachment) error
}

// ---------------------------------------------------------------------------
// SMTPProvider
// ---------------------------------------------------------------------------

// SMTPProvider delivers email by connecting to a remote SMTP server, upgrading
// to TLS via STARTTLS, authenticating with plain credentials, and injecting an
// RFC 822 / MIME message.
type SMTPProvider struct {
	host      string
	port      string
	username  string
	password  string
	fromEmail string
}

// NewSMTPProvider builds a provider for an SMTP delivery route.
func NewSMTPProvider(host, port, username, password, fromEmail string) *SMTPProvider {
	return &SMTPProvider{
		host:      host,
		port:      port,
		username:  username,
		password:  password,
		fromEmail: fromEmail,
	}
}

func (p *SMTPProvider) Send(to, subject, bodyHTML string, attachments []Attachment) error {
	addr := net.JoinHostPort(p.host, p.port)

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}

	client, err := smtp.NewClient(conn, p.host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp new client: %w", err)
	}

	// Greet and request STARTTLS when advertised.
	if err := client.Hello(p.host); err != nil {
		client.Close()
		return fmt.Errorf("smtp hello: %w", err)
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		tlsCfg := &tls.Config{ServerName: p.host}
		if err := client.StartTLS(tlsCfg); err != nil {
			client.Close()
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}

	// Authenticate.
	auth := smtp.PlainAuth("", p.username, p.password, p.host)
	if err := client.Auth(auth); err != nil {
		client.Close()
		return fmt.Errorf("smtp auth: %w", err)
	}

	// Envelope.
	if err := client.Mail(p.fromEmail); err != nil {
		client.Close()
		return fmt.Errorf("smtp mail from: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		client.Close()
		return fmt.Errorf("smtp rcpt to: %w", err)
	}

	// Body (RFC 822 / MIME multipart).
	w, err := client.Data()
	if err != nil {
		client.Close()
		return fmt.Errorf("smtp data: %w", err)
	}

	msg, err := p.buildMIMEMessage(to, subject, bodyHTML, attachments)
	if err != nil {
		client.Close()
		return fmt.Errorf("build MIME: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		client.Close()
		return fmt.Errorf("smtp data write: %w", err)
	}
	if err := w.Close(); err != nil {
		client.Close()
		return fmt.Errorf("smtp data close: %w", err)
	}

	return client.Quit()
}

// buildMIMEMessage constructs a multipart/mixed RFC 822 message.
// When there are no attachments it falls back to a single-part text/html
// message to maximise compatibility with strict MTAs.
func (p *SMTPProvider) buildMIMEMessage(to, subject, bodyHTML string, attachments []Attachment) ([]byte, error) {
	var buf bytes.Buffer

	if len(attachments) == 0 {
		// Simpler single-part message when there are no files.
		buf.WriteString(fmt.Sprintf("From: %s\r\n", p.fromEmail))
		buf.WriteString(fmt.Sprintf("To: %s\r\n", to))
		buf.WriteString(fmt.Sprintf("Subject: %s\r\n", subject))
		buf.WriteString("MIME-Version: 1.0\r\n")
		buf.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
		buf.WriteString("Content-Transfer-Encoding: 7bit\r\n")
		buf.WriteString("\r\n")
		buf.WriteString(bodyHTML)
		return buf.Bytes(), nil
	}

	// --- Multipart/mixed with HTML body + attachments -----------------------
	mpw := multipart.NewWriter(&buf)
	boundary := mpw.Boundary()

	buf.WriteString(fmt.Sprintf("From: %s\r\n", p.fromEmail))
	buf.WriteString(fmt.Sprintf("To: %s\r\n", to))
	buf.WriteString(fmt.Sprintf("Subject: %s\r\n", subject))
	buf.WriteString("MIME-Version: 1.0\r\n")
	buf.WriteString(fmt.Sprintf("Content-Type: multipart/mixed; boundary=%q\r\n", boundary))
	buf.WriteString("\r\n")

	// Part 1 — HTML body.
	htmlHdr := textproto.MIMEHeader{}
	htmlHdr.Set("Content-Type", "text/html; charset=\"UTF-8\"")
	htmlHdr.Set("Content-Transfer-Encoding", "7bit")
	part, err := mpw.CreatePart(htmlHdr)
	if err != nil {
		return nil, fmt.Errorf("create html part: %w", err)
	}
	if _, err := part.Write([]byte(bodyHTML)); err != nil {
		return nil, fmt.Errorf("write html part: %w", err)
	}

	// Parts 2..N — attachments.
	for _, att := range attachments {
		attHdr := textproto.MIMEHeader{}
		attHdr.Set("Content-Type", fmt.Sprintf("application/octet-stream; name=%q", att.FileName))
		attHdr.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", att.FileName))
		attHdr.Set("Content-Transfer-Encoding", "base64")

		part, err := mpw.CreatePart(attHdr)
		if err != nil {
			return nil, fmt.Errorf("create attachment part %s: %w", att.FileName, err)
		}
		// Base64-encode the raw bytes in chunks to avoid huge in-memory lines.
		enc := base64.NewEncoder(base64.StdEncoding, part)
		if _, err := enc.Write(att.Content); err != nil {
			enc.Close()
			return nil, fmt.Errorf("write attachment %s: %w", att.FileName, err)
		}
		enc.Close()
	}

	mpw.Close()
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// ResendProvider (API driver example)
// ---------------------------------------------------------------------------

const resendEndpoint = "https://api.resend.com/emails"

// ResendProvider delivers email via the Resend REST API using a raw API token.
type ResendProvider struct {
	apiKey    string
	fromEmail string
	client    *http.Client
}

// NewResendProvider builds a provider for a Resend API delivery route.
func NewResendProvider(apiKey, fromEmail string) *ResendProvider {
	return &ResendProvider{
		apiKey:    apiKey,
		fromEmail: fromEmail,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (p *ResendProvider) Send(to, subject, bodyHTML string, attachments []Attachment) error {
	// Build the JSON payload with an optional attachments array.
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf(`{"from":%q,"to":%q,"subject":%q,"html":%q`,
		p.fromEmail, to, subject, bodyHTML))

	if len(attachments) > 0 {
		buf.WriteString(`,"attachments":[`)
		for i, att := range attachments {
			if i > 0 {
				buf.WriteByte(',')
			}
			encoded := base64.StdEncoding.EncodeToString(att.Content)
			buf.WriteString(fmt.Sprintf(`{"filename":%q,"content":%q}`, att.FileName, encoded))
		}
		buf.WriteByte(']')
	}
	buf.WriteByte('}')

	req, err := http.NewRequest("POST", resendEndpoint, &buf)
	if err != nil {
		return fmt.Errorf("resend new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("resend http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("resend: unexpected status %s", resp.Status)
	}
	return nil
}
