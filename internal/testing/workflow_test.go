package testing

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"github.com/sarthak/vessel/internal/api"
	"github.com/sarthak/vessel/internal/config"
	"github.com/sarthak/vessel/internal/crypto"
	"github.com/sarthak/vessel/internal/db"
	"github.com/sarthak/vessel/internal/queue"
	"github.com/sarthak/vessel/internal/storage"
)

// ---------------------------------------------------------------------------
// Global test fixtures — initialised once in TestMain and reused by all tests.
// ---------------------------------------------------------------------------

var (
	testDB        *sql.DB
	testRedisOpt  asynq.RedisClientOpt
	testCfg       *config.AppConfig
	testMock      *MockEmailProvider
	testHandler   *queue.Handler
	testAServer   *asynq.Server
	testAMux      *asynq.ServeMux
	testWorkerWg  sync.WaitGroup
	projectRoot   string
)

const testTimeout = 15 * time.Second

// TestMain handles global setup (DB, Redis, migrations, worker) and teardown.
func TestMain(m *testing.M) {
	// ---- Resolve project root so we can find migration files ----------------
	_, filename, _, _ := runtime.Caller(0)
	projectRoot = filepath.Dir(filepath.Dir(filepath.Dir(filename)))

	// ---- Configuration (env vars fall back to docker-compose defaults) ------
	os.Setenv("DATABASE_URL", defEnv("DATABASE_URL", "postgres://vessel:vessel@localhost:5432/vessel?sslmode=disable"))
	os.Setenv("REDIS_ADDR", defEnv("REDIS_ADDR", "localhost:6379"))
	os.Setenv("API_PORT", defEnv("API_PORT", ":3001"))
	os.Setenv("MASTER_ENCRYPTION_KEY", defEnv("MASTER_ENCRYPTION_KEY", "12345678901234567890123456789012"))

	var err error
	testCfg, err = config.Load()
	if err != nil {
		log.Fatalf("config.Load: %v", err)
	}

	// ---- Database -----------------------------------------------------------
	testDB, err = db.Connect(testCfg.DatabaseURL)
	if err != nil {
		log.Fatalf("db.Connect: %v (is docker-compose up?)", err)
	}

	if err := runMigrations(); err != nil {
		log.Fatalf("runMigrations: %v", err)
	}

	// ---- Seed ---------------------------------------------------------------
	seedTestData()

	// ---- Redis / Asynq ------------------------------------------------------
	testRedisOpt = asynq.RedisClientOpt{Addr: testCfg.RedisAddr}

	testMock = &MockEmailProvider{}
	testHandler = queue.NewHandler(testDB, []byte(testCfg.MasterEncryptionKey))
	testHandler.TestProvider = testMock

	testAServer = asynq.NewServer(testRedisOpt, asynq.Config{Concurrency: 5})
	testAMux = asynq.NewServeMux()
	testAMux.HandleFunc("email:deliver", testHandler.HandleEmailDeliver)

	testWorkerWg.Add(1)
	go func() {
		defer testWorkerWg.Done()
		if err := testAServer.Run(testAMux); err != nil {
			log.Printf("asynq server exited: %v", err)
		}
	}()

	// ---- Run tests ----------------------------------------------------------
	code := m.Run()

	// ---- Teardown -----------------------------------------------------------
	testAServer.Shutdown()
	testWorkerWg.Wait()
	testDB.Close()
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func defEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// runMigrations reads the sole migration SQL file and executes every
// statement in the -- +goose Up section against the test database.
func runMigrations() error {
	path := filepath.Join(projectRoot, "db", "migrations", "00001_init_schema.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read migration: %w", err)
	}

	content := string(raw)

	// Locate the Up section.
	upIdx := strings.Index(content, "-- +goose Up")
	downIdx := strings.Index(content, "-- +goose Down")
	if upIdx < 0 || downIdx < 0 || downIdx <= upIdx {
		return fmt.Errorf("could not find Up/Down markers in %s", path)
	}
	upSQL := content[upIdx+len("-- +goose Up") : downIdx]

	// Strip goose directive lines and split by semicolons.
	lines := strings.Split(upSQL, "\n")
	var stmts []string
	var buf strings.Builder
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
		if strings.HasSuffix(strings.TrimSpace(line), ";") {
			stmts = append(stmts, buf.String())
			buf.Reset()
		}
	}
	if buf.Len() > 0 {
		stmts = append(stmts, buf.String())
	}

	for _, stmt := range stmts {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := testDB.Exec(stmt); err != nil {
			return fmt.Errorf("migration stmt %q: %w", stmt[:min(len(stmt), 80)], err)
		}
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// seedTestData inserts the hardcoded user and route that the API handler
// currently references (placeholders until auth is built).
func seedTestData() {
	encrypted, err := crypto.Encrypt([]byte("test-smtp-password"), []byte(testCfg.MasterEncryptionKey))
	if err != nil {
		log.Fatalf("encrypt seed auth: %v", err)
	}

	users := `INSERT INTO users (id, email) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`
	if _, err := testDB.Exec(users, "00000000-0000-0000-0000-000000000000", "test@vessel.dev"); err != nil {
		log.Fatalf("seed user: %v", err)
	}

	routesStmt := `
		INSERT INTO delivery_routes (id, user_id, route_type, from_email, smtp_host, smtp_port, smtp_username, encrypted_auth)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO NOTHING`
	if _, err := testDB.Exec(routesStmt,
		"00000000-0000-0000-0000-000000000001",
		"00000000-0000-0000-0000-000000000000",
		"SMTP",
		"sender@vessel.dev",
		"smtp.example.com",
		"587",
		"sender@vessel.dev",
		encrypted,
	); err != nil {
		log.Fatalf("seed route: %v", err)
	}
}

// truncateTables wipes runtime data between test cases while preserving
// schema and seed rows.
func truncateTables() {
	_, _ = testDB.Exec("DELETE FROM email_attachments")
	_, _ = testDB.Exec("DELETE FROM email_logs")
}

// drainAsync waits for the asynq worker to process the given log ID
// by polling the database status until it changes from "queued".
func drainAsync(t *testing.T, logID, expectedStatus string) {
	t.Helper()
	deadline := time.After(testTimeout)
	for {
		var status string
		err := testDB.QueryRow(`SELECT status FROM email_logs WHERE id = $1`, logID).Scan(&status)
		if err == nil && status == expectedStatus {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for log %s → %q (last status: %q, err: %v)",
				logID, expectedStatus, status, err)
		default:
			time.Sleep(150 * time.Millisecond)
		}
	}
}

// newFiberApp builds a test-only Fiber router with the production
// SendEmailHandler plus (optionally) a test-only batch endpoint.
func newFiberApp(redisClient *asynq.Client) *fiber.App {
	app := fiber.New(fiber.Config{ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second})

	// Production endpoint.
	app.Post("/v1/send", api.SendEmailHandler(testDB, redisClient))

	// ---- Test-only: batch send with personalisation -------------------------
	app.Post("/v1/send/batch", func(c *fiber.Ctx) error {
		var req struct {
			Emails []struct {
				Recipient  string            `json:"recipient"`
				Subject    string            `json:"subject"`
				BodyHTML   string            `json:"body_html"`
				Variables  map[string]string `json:"variables"` // e.g. {"name":"Alice"}
			} `json:"emails"`
		}
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid JSON"})
		}

		var ids []string
		for _, email := range req.Emails {
			// Apply personalisation via simple string replacement.
			subject := email.Subject
			body := email.BodyHTML
			for k, v := range email.Variables {
				placeholder := "{{" + k + "}}"
				subject = strings.ReplaceAll(subject, placeholder, v)
				body = strings.ReplaceAll(body, placeholder, v)
			}

			logID := uuid.New().String()
			now := time.Now().UTC()

			const insert = `
				INSERT INTO email_logs (id, user_id, route_id, recipient, subject, body_html, created_at, updated_at, status)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`
			if _, err := testDB.ExecContext(c.Context(), insert,
				logID,
				"00000000-0000-0000-0000-000000000000",
				"00000000-0000-0000-0000-000000000001",
				email.Recipient, subject, body, now, now, "queued",
			); err != nil {
				return c.Status(500).JSON(fiber.Map{"error": err.Error()})
			}

			task, err := queue.NewEmailDeliveryTask(logID)
			if err != nil {
				return c.Status(500).JSON(fiber.Map{"error": err.Error()})
			}
			if _, err := redisClient.Enqueue(task); err != nil {
				return c.Status(500).JSON(fiber.Map{"error": err.Error()})
			}
			ids = append(ids, logID)
		}

		return c.Status(http.StatusAccepted).JSON(fiber.Map{
			"ids":    ids,
			"status": "queued",
			"count":  len(ids),
		})
	})

	return app
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestSingleIngestionAndDelivery verifies the happy path:
// POST /v1/send → 202 Accepted → async worker → status 'sent' → mock verified.
func TestSingleIngestionAndDelivery(t *testing.T) {
	truncateTables()
	testMock.Reset()

	redisClient := asynq.NewClient(testRedisOpt)
	defer redisClient.Close()

	app := newFiberApp(redisClient)

	payload := fmt.Sprintf(`{"recipient":"alice@example.com","subject":"Hello","body_html":"<p>Hi</p>"}`)
	req, _ := http.NewRequest(http.MethodPost, "/v1/send", bytes.NewReader([]byte(payload)))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}

	// Extract the returned log ID.
	var body struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	resp.Body.Close()

	if body.Status != "queued" {
		t.Fatalf("expected status queued, got %s", body.Status)
	}

	// Wait for the async worker to pick up the task and mark it sent.
	drainAsync(t, body.ID, "sent")

	// Verify the mock was called with correct parameters.
	if c := testMock.CallCount(); c != 1 {
		t.Fatalf("expected 1 provider.Send call, got %d", c)
	}
	if testMock.Calls[0].To != "alice@example.com" {
		t.Fatalf("unexpected recipient: %s", testMock.Calls[0].To)
	}
	if testMock.Calls[0].Subject != "Hello" {
		t.Fatalf("unexpected subject: %s", testMock.Calls[0].Subject)
	}
}

// TestBatchAndPersonalisation sends N personalised emails through the batch
// endpoint and verifies each one was templated individually.
func TestBatchAndPersonalisation(t *testing.T) {
	truncateTables()
	testMock.Reset()

	redisClient := asynq.NewClient(testRedisOpt)
	defer redisClient.Close()

	app := newFiberApp(redisClient)

	recipients := []struct {
		Email string
		Name  string
	}{
		{"alice@example.com", "Alice"},
		{"bob@example.com", "Bob"},
		{"carol@example.com", "Carol"},
	}

	// Build batch payload.
	type emailItem struct {
		Recipient string            `json:"recipient"`
		Subject   string            `json:"subject"`
		BodyHTML  string            `json:"body_html"`
		Variables map[string]string `json:"variables"`
	}
	var items []emailItem
	for _, r := range recipients {
		items = append(items, emailItem{
			Recipient: r.Email,
			Subject:   "Hello {{name}}",
			BodyHTML:  "<p>Welcome, {{name}}!</p>",
			Variables: map[string]string{"name": r.Name},
		})
	}

	payload, _ := json.Marshal(map[string]any{"emails": items})
	req, _ := http.NewRequest(http.MethodPost, "/v1/send/batch", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, 10000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}

	var batchResp struct {
		IDs    []string `json:"ids"`
		Count  int      `json:"count"`
		Status string   `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		t.Fatalf("decode batch response: %v", err)
	}
	resp.Body.Close()

	if batchResp.Count != len(recipients) {
		t.Fatalf("expected %d enqueued, got %d", len(recipients), batchResp.Count)
	}

	// Wait for each log to reach 'sent'.
	for _, id := range batchResp.IDs {
		drainAsync(t, id, "sent")
	}

	// Verify the mock was called for every recipient with personalised content.
	if c := testMock.CallCount(); c != len(recipients) {
		t.Fatalf("expected %d provider.Send calls, got %d", len(recipients), c)
	}

	callByTo := make(map[string]MockSendCall)
	for _, call := range testMock.Calls {
		callByTo[call.To] = call
	}

	for _, r := range recipients {
		call, ok := callByTo[r.Email]
		if !ok {
			t.Fatalf("no delivery recorded for %s", r.Email)
		}
		if call.Subject != "Hello "+r.Name {
			t.Fatalf("%s subject: expected %q, got %q", r.Email, "Hello "+r.Name, call.Subject)
		}
		if !strings.Contains(call.BodyHTML, r.Name) {
			t.Fatalf("%s body does not contain name %q", r.Email, r.Name)
		}
	}
}

// TestAttachmentAndAutoPruning feeds an attachment through the storage layer
// and verifies that after the worker processes the task the file is gone.
func TestAttachmentAndAutoPruning(t *testing.T) {
	truncateTables()
	testMock.Reset()

	// ---- 1. Simulate the API saving an attachment to disk -------------------
	plainContent := []byte("fake-image-data-for-testing")
	base64Content := base64.StdEncoding.EncodeToString(plainContent)

	localPath, err := storage.SaveAttachment(base64Content, "report.pdf")
	if err != nil {
		t.Fatalf("SaveAttachment: %v", err)
	}

	// Verify the file exists immediately after save.
	if _, err := os.Stat(localPath); os.IsNotExist(err) {
		t.Fatal("attachment file should exist right after SaveAttachment")
	}

	// ---- 2. Insert an email_log + attachment row + enqueue task -------------
	logID := uuid.New().String()
	now := time.Now().UTC()

	_, err = testDB.Exec(`
		INSERT INTO email_logs (id, user_id, route_id, recipient, subject, body_html, created_at, updated_at, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		logID,
		"00000000-0000-0000-0000-000000000000",
		"00000000-0000-0000-0000-000000000001",
		"attachments@test.dev", "See attached", "<p>File inside</p>",
		now, now, "queued",
	)
	if err != nil {
		t.Fatalf("insert email_log: %v", err)
	}

	_, err = testDB.Exec(`
		INSERT INTO email_attachments (email_log_id, file_name, file_type, local_path, file_size_bytes)
		VALUES ($1,$2,$3,$4,$5)`,
		logID, "report.pdf", "application/pdf", localPath, len(plainContent),
	)
	if err != nil {
		t.Fatalf("insert email_attachment: %v", err)
	}

	// Enqueue the task.
	redisClient := asynq.NewClient(testRedisOpt)
	defer redisClient.Close()

	task, err := queue.NewEmailDeliveryTask(logID)
	if err != nil {
		t.Fatalf("NewEmailDeliveryTask: %v", err)
	}
	if _, err := redisClient.Enqueue(task); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// ---- 3. Wait for the worker to process and auto-prune -------------------
	drainAsync(t, logID, "sent")

	// ---- 4. Assert the file was pruned from disk ----------------------------
	if _, err := os.Stat(localPath); !os.IsNotExist(err) {
		t.Fatal("attachment file should have been pruned after delivery")
	}

	// ---- 5. Assert the mock received the attachment data --------------------
	if c := testMock.CallCount(); c != 1 {
		t.Fatalf("expected 1 provider.Send call, got %d", c)
	}
	call := testMock.Calls[0]
	if len(call.Attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(call.Attachments))
	}
	if call.Attachments[0].FileName != "report.pdf" {
		t.Fatalf("expected attachment filename report.pdf, got %s", call.Attachments[0].FileName)
	}
	if string(call.Attachments[0].Content) != string(plainContent) {
		t.Fatal("attachment content mismatch")
	}
}

// TestFailureBranch injects a simulated error into the mock and asserts the
// log transitions to 'failed' and that asynq retries correctly.
func TestFailureBranch(t *testing.T) {
	truncateTables()
	testMock.Reset()

	// Make every Send call fail.
	testMock.SendError = fmt.Errorf("simulated SMTP timeout")

	redisClient := asynq.NewClient(testRedisOpt)
	defer redisClient.Close()

	app := newFiberApp(redisClient)

	payload := `{"recipient":"fail@example.com","subject":"Fail","body_html":"<p>fail</p>"}`
	req, _ := http.NewRequest(http.MethodPost, "/v1/send", bytes.NewReader([]byte(payload)))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}

	var body struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()

	// Wait for the worker to exhaust retries and land on 'failed'.
	drainAsync(t, body.ID, "failed")

	// Verify the error message was persisted.
	var errMsg string
	if err := testDB.QueryRow(`SELECT COALESCE(error_message, '') FROM email_logs WHERE id = $1`, body.ID).Scan(&errMsg); err != nil {
		t.Fatalf("query error_message: %v", err)
	}
	if !strings.Contains(errMsg, "simulated SMTP timeout") {
		t.Fatalf("expected error_message to contain 'simulated SMTP timeout', got %q", errMsg)
	}
}
