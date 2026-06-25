package routes

// smtp_stream.go — memory-bounded MIME streaming for the SMTPProvider.
//
// Architecture
// ────────────
// The original provider.go builds the entire MIME message in a bytes.Buffer
// before writing it to the socket. For a 20 MB PDF attachment that means
// ≥ 20 MB of heap pressure per concurrent delivery goroutine.
//
// This file replaces that code path with a zero-copy pipeline:
//
//   RFC-822 headers ──┐
//   HTML body string ─┤──► multipart.Writer ──► smtp.Data() writer ──► TCP socket
//   attachment FDs ───┘         (32 KB buf)
//
// At any point in time the worker holds at most one 32 KB copy buffer in RAM,
// regardless of the number or size of attachments.
//
// Integration
// ───────────
// Call SMTPProvider.sendStream() instead of SMTPProvider.Send() when the
// caller has on-disk attachment paths rather than pre-loaded []byte content.
// The existing Send() method (used by the mock in tests) is preserved
// unchanged in provider.go so that the test suite does not need to change.

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"time"
)

// smtpCopyBuf is the size of the per-attachment copy buffer.
// 32 KB keeps stack pressure low while amortising syscall overhead well.
const smtpCopyBuf = 32 * 1024

// DiskAttachment is an on-disk attachment reference used by SendStream.
// The worker passes these instead of loading file bytes into memory first.
type DiskAttachment struct {
	FileName  string // original filename shown to the recipient
	LocalPath string // absolute path on the worker's filesystem
}

// SendStream dials the SMTP server, authenticates, and streams the full
// MIME message directly into the open socket writer without buffering the
// message body or attachment bytes in RAM.
//
// Resource lifecycle (all via defer, outermost-first teardown):
//  1. smtp.Client.Quit() + Close()  — SMTP session
//  2. multipart.Writer.Close()      — writes the terminal boundary
//  3. smtp.Data writer.Close()      — signals end of DATA command to the MTA
//  4. Each os.File.Close()          — attachment file descriptors
func (p *SMTPProvider) SendStream(to, subject, bodyHTML string, attachments []DiskAttachment) error {
	// ── 1. Dial ──────────────────────────────────────────────────────────────
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
	// Ensure the SMTP client (and underlying TCP conn) is always closed.
	defer client.Close()

	// ── 2. STARTTLS upgrade ───────────────────────────────────────────────────
	if err := client.Hello(p.host); err != nil {
		return fmt.Errorf("smtp hello: %w", err)
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		tlsCfg := &tls.Config{ServerName: p.host}
		if err := client.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}

	// ── 3. Authenticate ───────────────────────────────────────────────────────
	auth := smtp.PlainAuth("", p.username, p.password, p.host)
	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("smtp auth: %w", err)
	}

	// ── 4. Envelope ───────────────────────────────────────────────────────────
	if err := client.Mail(p.fromEmail); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("smtp rcpt to: %w", err)
	}

	// ── 5. DATA writer ────────────────────────────────────────────────────────
	// w is the raw io.WriteCloser that maps directly onto the TCP socket.
	// Everything written here goes directly to the MTA over the wire.
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	// Close the DATA command writer before Quit — the MTA expects the "." CRLF
	// terminator that w.Close() emits.
	defer w.Close()

	// ── 6. Stream the MIME message ────────────────────────────────────────────
	if len(attachments) == 0 {
		return streamSimpleMessage(w, p.fromEmail, to, subject, bodyHTML)
	}
	return streamMultipartMessage(w, p.fromEmail, to, subject, bodyHTML, attachments)
}

// streamSimpleMessage writes a plain text/html RFC-822 message when there are
// no attachments. No multipart framing is needed, so there is zero overhead
// beyond the fixed-size headers.
func streamSimpleMessage(w io.Writer, from, to, subject, bodyHTML string) error {
	hdr := fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\n"+
			"Content-Type: text/html; charset=\"UTF-8\"\r\n"+
			"Content-Transfer-Encoding: 7bit\r\n\r\n",
		from, to, subject,
	)
	if _, err := io.WriteString(w, hdr); err != nil {
		return fmt.Errorf("write headers: %w", err)
	}
	if _, err := io.WriteString(w, bodyHTML); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	return nil
}

// streamMultipartMessage opens each attachment file, then constructs a
// multipart/mixed message by writing parts sequentially into the DATA writer.
//
// Data flow per attachment:
//
//	os.File (disk FD) ──► base64.NewEncoder(part writer) ──► multipart boundary ──► socket
//
// The 32 KB io.CopyBuffer buffer is reused across all parts, so total
// allocation is O(1) with respect to file size and attachment count.
func streamMultipartMessage(
	w io.Writer,
	from, to, subject, bodyHTML string,
	attachments []DiskAttachment,
) error {
	// Create the multipart writer backed directly by the socket writer.
	// multipart.NewWriter wraps w with no internal buffer of its own — it
	// passes through every Write call immediately.
	mpw := multipart.NewWriter(w)

	// Write RFC-822 headers first, before any multipart boundaries.
	// The boundary must be known before writing the top-level Content-Type.
	hdr := fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\n"+
			"Content-Type: multipart/mixed; boundary=%q\r\n\r\n",
		from, to, subject, mpw.Boundary(),
	)
	if _, err := io.WriteString(w, hdr); err != nil {
		return fmt.Errorf("write headers: %w", err)
	}

	// Part 1 — HTML body.
	htmlHdr := textproto.MIMEHeader{}
	htmlHdr.Set("Content-Type", `text/html; charset="UTF-8"`)
	htmlHdr.Set("Content-Transfer-Encoding", "7bit")

	htmlPart, err := mpw.CreatePart(htmlHdr)
	if err != nil {
		return fmt.Errorf("create html part: %w", err)
	}
	if _, err := io.WriteString(htmlPart, bodyHTML); err != nil {
		return fmt.Errorf("write html part: %w", err)
	}

	// Parts 2..N — on-disk attachments.
	// We use a single reusable copy buffer across all parts to keep total
	// allocation constant at 32 KB regardless of the number of attachments.
	copyBuf := make([]byte, smtpCopyBuf)

	for _, att := range attachments {
		if err := streamAttachmentPart(mpw, att, copyBuf); err != nil {
			return err
		}
	}

	// Write the terminal "--boundary--" line and flush any buffered state.
	// This must happen before w.Close() (called by the caller's defer).
	if err := mpw.Close(); err != nil {
		return fmt.Errorf("close multipart writer: %w", err)
	}

	return nil
}

// streamAttachmentPart opens a single file from disk and streams its content,
// base64-encoded, directly into a new multipart part without buffering.
//
// Resource lifecycle:
//  1. os.File is always closed via defer, even if base64.Encoder.Write fails.
//  2. base64.Encoder is always flushed via enc.Close() — critical because the
//     encoder holds up to 2 bytes in its internal accumulator and only emits
//     them (with padding) when Close() is called. Skipping this produces a
//     corrupt attachment.
func streamAttachmentPart(mpw *multipart.Writer, att DiskAttachment, copyBuf []byte) error {
	f, err := os.Open(filepath.Clean(att.LocalPath))
	if err != nil {
		return fmt.Errorf("open attachment %s: %w", att.FileName, err)
	}
	defer f.Close()

	attHdr := textproto.MIMEHeader{}
	attHdr.Set("Content-Type", fmt.Sprintf("application/octet-stream; name=%q", att.FileName))
	attHdr.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", att.FileName))
	// base64 is mandatory for binary MIME parts transmitted over SMTP.
	attHdr.Set("Content-Transfer-Encoding", "base64")

	part, err := mpw.CreatePart(attHdr)
	if err != nil {
		return fmt.Errorf("create attachment part %s: %w", att.FileName, err)
	}

	// base64.NewEncoder wraps the part writer (which itself wraps the socket).
	// Every 3 raw bytes it reads from the file become exactly 4 base64 characters
	// written to part (and therefore to the socket) immediately.
	enc := base64.NewEncoder(base64.StdEncoding, part)
	defer enc.Close() // flushes trailing bytes + padding; must not be skipped

	if _, err := io.CopyBuffer(enc, f, copyBuf); err != nil {
		return fmt.Errorf("stream attachment %s: %w", att.FileName, err)
	}

	// Explicit enc.Close() is called by defer above even if CopyBuffer
	// returns an error, ensuring the encoder always attempts to flush.
	return nil
}
