package api

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/sarthak/vessel/internal/queue"
)

// sendRequest is the shape accepted by POST /v1/send.
type sendRequest struct {
	Recipient string `json:"recipient" validate:"required"`
	Subject   string `json:"subject"   validate:"required"`
	BodyHTML  string `json:"body_html" validate:"required"`
}

// sendResponse is returned on a successful enqueue (HTTP 202).
type sendResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// SendEmailHandler returns a Fiber handler that:
//  1. validates the JSON body,
//  2. writes a "queued" row to email_logs,
//  3. enqueues the log ID to Redis via asynq,
//  4. returns 202 Accepted.
func SendEmailHandler(db *sql.DB, redisClient *asynq.Client) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var req sendRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "invalid JSON body",
			})
		}

		// Basic required-field checks (Fiber's BodyParser won't enforce "required" tags).
		if req.Recipient == "" || req.Subject == "" || req.BodyHTML == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "recipient, subject, and body_html are required",
			})
		}

		logID := uuid.New().String()
		now := time.Now().UTC()

		// --- Persist the log entry -------------------------------------------------
		//
		// TODO: Replace hardcoded user_id / route_id with values from auth context
		//       once the authentication layer is built.
		const insertSQL = `
			INSERT INTO email_logs (id, user_id, route_id, recipient, subject, body_html, created_at, updated_at, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

		if _, err := db.ExecContext(
			c.Context(),
			insertSQL,
			logID,
			"00000000-0000-0000-0000-000000000000", // placeholder user_id
			"00000000-0000-0000-0000-000000000001", // placeholder route_id
			req.Recipient,
			req.Subject,
			req.BodyHTML,
			now,
			now,
			"queued",
		); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "failed to persist email log",
			})
		}

		// --- Enqueue the delivery task ---------------------------------------------
		task, err := queue.NewEmailDeliveryTask(logID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "failed to serialise delivery task",
			})
		}

		if _, err := redisClient.Enqueue(task); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "failed to enqueue delivery task",
			})
		}

		// --- Respond 202 -----------------------------------------------------------
		return c.Status(http.StatusAccepted).JSON(sendResponse{
			ID:     logID,
			Status: "queued",
		})
	}
}
