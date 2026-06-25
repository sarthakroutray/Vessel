package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/hibiken/asynq"
	"github.com/sarthak/vessel/internal/crypto"
	"github.com/sarthak/vessel/internal/routes"
	"github.com/sarthak/vessel/internal/storage"
)

// Handler holds the dependencies the asynq task handler needs.
type Handler struct {
	DB        *sql.DB
	CryptoKey []byte // the 32-byte master encryption key

	// TestProvider, when non-nil, is used instead of the route_type switch
	// so integration tests can inject a mock without touching production code.
	TestProvider routes.EmailProvider
}

// NewHandler creates a Handler ready to be registered with an asynq mux.
func NewHandler(db *sql.DB, key []byte) *Handler {
	return &Handler{DB: db, CryptoKey: key}
}

// HandleEmailDeliver is the asynq handler for the "email:deliver" task.
func (h *Handler) HandleEmailDeliver(ctx context.Context, t *asynq.Task) error {
	// 1. Unmarshal task payload.
	var p EmailTaskPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("unmarshal payload: %w", err)
	}

	// 2. Fetch the email log row.
	logEntry, err := queryEmailLog(ctx, h.DB, p.LogID)
	if err != nil {
		return fmt.Errorf("query email_log: %w", err)
	}

	// 3. Fetch the associated delivery route.
	route, err := queryDeliveryRoute(ctx, h.DB, logEntry.RouteID)
	if err != nil {
		return fmt.Errorf("query delivery_route: %w", err)
	}

	// 4. Decrypt the stored credential.
	plaintext, err := crypto.Decrypt(route.EncryptedAuth, h.CryptoKey)
	if err != nil {
		return fmt.Errorf("decrypt route auth: %w", err)
	}
	decryptedSecret := string(plaintext)

	// 5. Load attachments from disk.
	attachRows, err := queryAttachments(ctx, h.DB, p.LogID)
	if err != nil {
		return fmt.Errorf("query attachments: %w", err)
	}

	attachments := make([]routes.Attachment, 0, len(attachRows))
	// Track every file path we read so we can prune them in the defer below,
	// regardless of whether Send succeeds or fails.
	var diskPaths []string

	for _, ar := range attachRows {
		data, err := storage.ReadFile(ar.LocalPath)
		if err != nil {
			// If we can't read a file off disk, fail early — no point sending
			// an email with missing attachments.
			return fmt.Errorf("read attachment %s: %w", ar.LocalPath, err)
		}
		attachments = append(attachments, routes.Attachment{
			FileName: ar.FileName,
			Content:  data,
		})
		diskPaths = append(diskPaths, ar.LocalPath)
	}

	// 6. Guarantee disk cleanup after delivery attempt.
	defer func() {
		for _, path := range diskPaths {
			if err := storage.PruneFile(path); err != nil {
				log.Printf("WARN: prune file %s: %v", path, err)
			}
		}
	}()

	// 7. Build the correct EmailProvider.
	//
	// In production the route_type drives the switch; in integration tests
	// TestProvider is set externally to intercept the call.
	provider := h.TestProvider
	if provider == nil {
		switch route.RouteType {
		case "SMTP":
			provider = routes.NewSMTPProvider(
				route.SMTPHost,
				route.SMTPPort,
				route.SMTPUsername,
				decryptedSecret,
				route.FromEmail,
			)
		case "API":
			provider = routes.NewResendProvider(decryptedSecret, route.FromEmail)
		default:
			return fmt.Errorf("unknown route_type: %s", route.RouteType)
		}
	}

	// 8. Send — the defer above will clean up disk regardless of outcome.
	if err := provider.Send(logEntry.Recipient, logEntry.Subject, logEntry.BodyHTML, attachments); err != nil {
		if updateErr := updateLogStatus(ctx, h.DB, p.LogID, "failed", err.Error()); updateErr != nil {
			log.Printf("WARN: failed to update log %s to 'failed': %v", p.LogID, updateErr)
		}
		return fmt.Errorf("provider.Send: %w", err)
	}

	// 9. Mark sent.
	if err := updateLogStatus(ctx, h.DB, p.LogID, "sent", ""); err != nil {
		return fmt.Errorf("update log sent: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Database helpers
// ---------------------------------------------------------------------------

type emailLogRow struct {
	RouteID   string
	Recipient string
	Subject   string
	BodyHTML  string
}

func queryEmailLog(ctx context.Context, db *sql.DB, logID string) (*emailLogRow, error) {
	const q = `SELECT route_id, recipient, subject, body_html FROM email_logs WHERE id = $1`
	var r emailLogRow
	if err := db.QueryRowContext(ctx, q, logID).Scan(
		&r.RouteID, &r.Recipient, &r.Subject, &r.BodyHTML,
	); err != nil {
		return nil, fmt.Errorf("email_logs lookup (%s): %w", logID, err)
	}
	return &r, nil
}

type deliveryRouteRow struct {
	RouteType     string
	FromEmail     string
	SMTPHost      string
	SMTPPort      string
	SMTPUsername  string
	EncryptedAuth []byte
}

func queryDeliveryRoute(ctx context.Context, db *sql.DB, routeID string) (*deliveryRouteRow, error) {
	const q = `
		SELECT route_type, from_email,
		       COALESCE(smtp_host, ''),
		       COALESCE(smtp_port, ''),
		       COALESCE(smtp_username, ''),
		       encrypted_auth
		FROM delivery_routes
		WHERE id = $1`

	var r deliveryRouteRow
	if err := db.QueryRowContext(ctx, q, routeID).Scan(
		&r.RouteType,
		&r.FromEmail,
		&r.SMTPHost,
		&r.SMTPPort,
		&r.SMTPUsername,
		&r.EncryptedAuth,
	); err != nil {
		return nil, fmt.Errorf("delivery_routes lookup (%s): %w", routeID, err)
	}
	return &r, nil
}

type attachmentRow struct {
	FileName  string
	LocalPath string
}

func queryAttachments(ctx context.Context, db *sql.DB, logID string) ([]attachmentRow, error) {
	const q = `SELECT file_name, local_path FROM email_attachments WHERE email_log_id = $1 ORDER BY created_at`
	rows, err := db.QueryContext(ctx, q, logID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []attachmentRow
	for rows.Next() {
		var r attachmentRow
		if err := rows.Scan(&r.FileName, &r.LocalPath); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func updateLogStatus(ctx context.Context, db *sql.DB, logID, status, errMsg string) error {
	const q = `UPDATE email_logs SET status = $1, error_message = $2, updated_at = $3 WHERE id = $4`
	_, err := db.ExecContext(ctx, q, status, errMsg, time.Now().UTC(), logID)
	return err
}
