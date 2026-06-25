# Vessel 🚢

> An asynchronous, memory-bounded transactional email API and background worker service built in Go.

Vessel accepts email requests over HTTP, persists them to PostgreSQL, and offloads delivery to background workers via Redis. Attachments — including large PDFs, images, and archives — are handled with a streaming architecture that keeps worker RAM usage flat at ≈ 32 KB per delivery, regardless of file size.

---

## Features

| Feature | Detail |
| :--- | :--- |
| **Async Queuing** | Fiber HTTP API enqueues tasks to Redis via [Asynq](https://github.com/hibiken/asynq), returning `202 Accepted` immediately |
| **Streaming Attachments** | Base64 payloads decoded and written to disk chunk-by-chunk; never fully materialised in RAM |
| **Streaming SMTP Delivery** | Attachment files piped from disk directly into the SMTP socket via `io.CopyBuffer`; ≈ 32 KB peak RAM per delivery |
| **Request Size Limiting** | Hard 25 MB ceiling enforced at the middleware layer before any handler runs |
| **Multiple Providers** | Pluggable `EmailProvider` interface — SMTP (streaming) and Resend REST API included |
| **AES-256-GCM Credentials** | SMTP passwords and API keys encrypted at rest; decrypted in-memory only during delivery |
| **Dynamic Personalisation** | Per-recipient variable interpolation (`{{name}}`, `{{order_id}}`, …) in subjects and HTML bodies |
| **Ephemeral Storage** | Attachment files auto-pruned from disk after delivery, whether the send succeeds or fails |
| **Database Migrations** | Versioned schema via [Goose](https://github.com/pressly/goose) |

---

## Architecture

```mermaid
flowchart TD
    Client["Client App"] -->|"1. POST /v1/send (JSON + base64 attachments)"| MW["RequestSizeLimit Middleware (25 MB hard cap)"]
    MW --> API["API Handler (Fiber)"]
    API -->|"2. SaveAttachmentStream() — 32 KB chunks → disk"| Disk[("./tmp/attachments")]
    API -->|"3. INSERT email_logs status='queued'"| DB[("PostgreSQL")]
    API -->|"4. Enqueue task (log ID only)"| Broker[("Redis / Asynq")]
    API -->|"5. 202 Accepted"| Client

    Worker["Worker (Asynq)"] -->|"6. Dequeue task"| Broker
    Worker -->|"7. Fetch log + route"| DB
    Worker -->|"8. AES-256-GCM decrypt"| Crypto["Master Key"]
    Worker -->|"9. SendStream() — file FDs → socket"| SMTP["SMTP Server"]
    Worker -->|"10. UPDATE status='sent'/'failed'"| DB
    Worker -->|"11. PruneFile() — remove from disk"| Disk
```

### Memory Profile

| Operation | Old (buffered) | New (streaming) |
| :--- | :--- | :--- |
| Save 20 MB PDF from request | +20 MB heap | ≈ 32 KB copy buffer |
| SMTP delivery of 20 MB PDF | +20 MB heap (base64 in buf) | ≈ 32 KB copy buffer |
| Concurrent request limit | Unbounded | Capped at 25 MB per request |

---

## Technical Stack

- **Runtime**: Go 1.24+
- **HTTP Framework**: [Fiber v2](https://gofiber.io/)
- **Task Queue**: [Asynq](https://github.com/hibiken/asynq) backed by Redis 7
- **Database**: PostgreSQL 15 (`lib/pq` driver)
- **Migrations**: [Goose](https://github.com/pressly/goose)
- **Encryption**: AES-256-GCM (`crypto/cipher` stdlib)
- **MIME Streaming**: `mime/multipart` + `encoding/base64` + `io.CopyBuffer`

---

## Getting Started

### Prerequisites

- Go 1.24 or later
- Docker and Docker Compose (for local infrastructure)

### 1. Clone and start infrastructure

```bash
git clone https://github.com/sarthakroutray/Vessel.git
cd Vessel
make up          # starts PostgreSQL 15 + Redis 7 via docker-compose
```

### 2. Apply database migrations

```bash
make migrate
```

### 3. Configure environment

Copy the values below into your shell or a `.env` file:

```bash
export DATABASE_URL="postgres://vessel:vessel@127.0.0.1:5432/vessel?sslmode=disable"
export REDIS_ADDR="127.0.0.1:6379"
export API_PORT=":3000"
export MASTER_ENCRYPTION_KEY="12345678901234567890123456789012"  # must be exactly 32 bytes
```

### 4. Run the services

Open two terminals:

```bash
# Terminal 1 — HTTP API
go run cmd/api/main.go

# Terminal 2 — background delivery worker
go run cmd/worker/main.go
```

### 5. Run the test suite

```bash
make test
```

---

## Environment Variables

| Variable | Required | Description |
| :--- | :---: | :--- |
| `DATABASE_URL` | ✅ | Full PostgreSQL DSN |
| `REDIS_ADDR` | ✅ | `host:port` of the Redis instance |
| `API_PORT` | ✅ | Listening port for the HTTP API (e.g. `:3000`) |
| `MASTER_ENCRYPTION_KEY` | ✅ | Exactly 32 ASCII bytes used as the AES-256 master key |

---

## API Reference

### Request limits

Every route is protected by a global middleware that enforces a **25 MB maximum body size**. Requests exceeding this limit receive:

```http
HTTP/1.1 413 Request Entity Too Large
Content-Type: application/json

{"error": "request body exceeds the 25 MB limit"}
```

---

### POST `/v1/send` — Send a single email

Enqueues one transactional email for delivery.

**Headers**

```
Content-Type: application/json
```

**Body fields**

| Field | Type | Required | Description |
| :--- | :--- | :---: | :--- |
| `recipient` | string | ✅ | Destination email address |
| `subject` | string | ✅ | Email subject line |
| `body_html` | string | ✅ | Full HTML body |

#### Example — plain email

```bash
curl -X POST http://localhost:3000/v1/send \
  -H "Content-Type: application/json" \
  -d '{
    "recipient": "alice@example.com",
    "subject":   "Your invoice is ready",
    "body_html": "<h1>Hi Alice</h1><p>Your invoice #1042 is attached.</p>"
  }'
```

```json
{
  "id":     "e30b1dbf-7f72-4d2d-9475-7b56f8f53a81",
  "status": "queued"
}
```

#### Example — email with a PDF attachment

Attachments are sent as a base64-encoded string. You can encode any file from the command line:

```bash
# 1. Encode your file
BASE64_PDF=$(base64 -w 0 invoice_1042.pdf)

# 2. Send the request
curl -X POST http://localhost:3000/v1/send \
  -H "Content-Type: application/json" \
  -d "{
    \"recipient\": \"alice@example.com\",
    \"subject\":   \"Your invoice is ready\",
    \"body_html\": \"<h1>Hi Alice</h1><p>Please find your invoice attached.</p>\",
    \"attachments\": [
      {
        \"file_name\": \"invoice_1042.pdf\",
        \"file_type\": \"application/pdf\",
        \"content\":   \"${BASE64_PDF}\"
      }
    ]
  }"
```

```json
{
  "id":     "a1b2c3d4-0000-0000-0000-ffffffffffff",
  "status": "queued"
}
```

> **How large attachments are handled**
>
> The API handler calls `storage.SaveAttachmentStream()` which pipes the base64
> payload through `base64.NewDecoder` → `io.CopyBuffer` (32 KB buffer) → an
> `os.File` on disk. The decoded bytes are **never buffered in RAM**. The worker
> later opens the file by path and streams it directly into the SMTP socket using
> `SMTPProvider.SendStream()`. After delivery the file is deleted from disk.

#### Example — email with multiple attachments

```bash
BASE64_PDF=$(base64 -w 0 report.pdf)
BASE64_CSV=$(base64 -w 0 data.csv)

curl -X POST http://localhost:3000/v1/send \
  -H "Content-Type: application/json" \
  -d "{
    \"recipient\": \"finance@example.com\",
    \"subject\":   \"Q2 Report Package\",
    \"body_html\": \"<p>Please find the Q2 report and raw data attached.</p>\",
    \"attachments\": [
      {
        \"file_name\": \"Q2_report.pdf\",
        \"file_type\": \"application/pdf\",
        \"content\":   \"${BASE64_PDF}\"
      },
      {
        \"file_name\": \"Q2_data.csv\",
        \"file_type\": \"text/csv\",
        \"content\":   \"${BASE64_CSV}\"
      }
    ]
  }"
```

```json
{
  "id":     "d4e5f6a7-1111-2222-3333-444444444444",
  "status": "queued"
}
```

#### Example — large HTML email body

There is no separate limit on `body_html` size beyond the 25 MB total request cap. Rich transactional emails with inlined CSS are fully supported:

```bash
curl -X POST http://localhost:3000/v1/send \
  -H "Content-Type: application/json" \
  -d '{
    "recipient": "newsletter@example.com",
    "subject":   "Your Monthly Digest",
    "body_html": "<html><head><style>body{font-family:sans-serif;background:#f4f4f4}...</style></head><body><h1>June Digest</h1><!-- full newsletter HTML --></body></html>"
  }'
```

---

### POST `/v1/send/batch` — Send personalised batch emails

Enqueues multiple personalised emails in a single request. Variable placeholders in `subject` and `body_html` are substituted per-recipient before enqueueing.

**Body fields**

| Field | Type | Required | Description |
| :--- | :--- | :---: | :--- |
| `emails` | array | ✅ | List of email objects |
| `emails[].recipient` | string | ✅ | Recipient address |
| `emails[].subject` | string | ✅ | Subject (supports `{{variable}}` tokens) |
| `emails[].body_html` | string | ✅ | HTML body (supports `{{variable}}` tokens) |
| `emails[].variables` | object | — | Key/value map substituted into subject and body |

#### Example — personalised batch

```bash
curl -X POST http://localhost:3000/v1/send/batch \
  -H "Content-Type: application/json" \
  -d '{
    "emails": [
      {
        "recipient":  "alice@example.com",
        "subject":    "Hi {{name}}, your order {{order_id}} shipped!",
        "body_html":  "<p>Dear {{name}},</p><p>Order <strong>{{order_id}}</strong> is on its way.</p>",
        "variables":  { "name": "Alice", "order_id": "ORD-1001" }
      },
      {
        "recipient":  "bob@example.com",
        "subject":    "Hi {{name}}, your order {{order_id}} shipped!",
        "body_html":  "<p>Dear {{name}},</p><p>Order <strong>{{order_id}}</strong> is on its way.</p>",
        "variables":  { "name": "Bob", "order_id": "ORD-1002" }
      },
      {
        "recipient":  "carol@example.com",
        "subject":    "Hi {{name}}, your order {{order_id}} shipped!",
        "body_html":  "<p>Dear {{name}},</p><p>Order <strong>{{order_id}}</strong> is on its way.</p>",
        "variables":  { "name": "Carol", "order_id": "ORD-1003" }
      }
    ]
  }'
```

```json
{
  "ids": [
    "4d2091fb-55f6-455b-b9d9-0c6d59b21f92",
    "9d18e957-c875-40e1-bbcb-875f6bc9a752",
    "b3c4d5e6-f7a8-9012-bcde-f01234567890"
  ],
  "status": "queued",
  "count": 3
}
```

---

## Delivery Status Lifecycle

Once a task is enqueued the log row in PostgreSQL transitions through these states:

```
queued  ──► (worker picks up task)
              ├──► sent    — provider.Send() returned nil
              └──► failed  — provider.Send() returned an error
                              (error_message column populated)
```

Query delivery status directly:

```sql
SELECT id, recipient, subject, status, error_message, updated_at
FROM   email_logs
WHERE  id = 'e30b1dbf-7f72-4d2d-9475-7b56f8f53a81';
```

---

## Makefile Reference

| Target | Description |
| :--- | :--- |
| `make up` | Start PostgreSQL + Redis via `docker compose up -d` |
| `make down` | Stop and remove containers and volumes |
| `make migrate` | Apply all pending Goose migrations |
| `make migrate-down` | Roll back the last migration |
| `make migrate-reset` | Roll back all migrations |
| `make build` | Compile all binaries (`go build ./...`) |
| `make test` | Run the full integration test suite |
| `make create name="<name>"` | Scaffold a new SQL migration file |

---

## Project Structure

```
Vessel/
├── cmd/
│   ├── api/          — HTTP API server entrypoint
│   └── worker/       — Asynq background worker entrypoint
├── db/
│   └── migrations/   — Goose SQL migration files
├── internal/
│   ├── api/
│   │   ├── handlers.go     — POST /v1/send handler
│   │   └── middleware.go   — RequestSizeLimit (25 MB cap)
│   ├── config/       — Environment variable loading
│   ├── crypto/       — AES-256-GCM encrypt / decrypt
│   ├── db/           — PostgreSQL connection pool
│   ├── queue/
│   │   ├── tasks.go        — Asynq task definition
│   │   └── worker.go       — HandleEmailDeliver (streaming dispatch)
│   ├── routes/
│   │   ├── provider.go     — EmailProvider interface + ResendProvider
│   │   └── smtp_stream.go  — SMTPProvider.SendStream() (zero-copy SMTP)
│   ├── storage/
│   │   ├── local.go        — PruneFile, ReadFile helpers
│   │   └── stream.go       — SaveAttachmentStream() (zero-copy save)
│   └── testing/
│       ├── mocks.go        — MockEmailProvider test spy
│       └── workflow_test.go — End-to-end integration tests
└── docker-compose.yml
```
