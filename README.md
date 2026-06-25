# Vessel 🚢

An asynchronous transactional email API and background worker service built in Go. Vessel enables fast, reliable, and secure email dispatch by offloading delivery to background queues.

## Features

- **Asynchronous Email Queuing**: Fiber-based HTTP API enqueues delivery requests immediately to Redis via [Asynq](https://github.com/hibiken/asynq), returning `202 Accepted` to clients.
- **Background Delivery Workers**: Independent concurrent worker processes consume tasks, handle network retries, and update delivery statuses.
- **Multiple Providers**: Standardized `EmailProvider` interface supporting standard SMTP routes and HTTP REST APIs (e.g., [Resend](https://resend.com/)).
- **Secure Credentials**: Delivery route credentials (SMTP passwords, API keys) are encrypted at rest using AES-256-GCM with a master key and only decrypted in-memory during delivery.
- **Dynamic Personalization**: Support for batch personalization endpoints with simple variable interpolation (e.g. `{{name}}`).
- **Ephemeral Attachments**: Temporary disk storage for email attachments with automatic pruning upon successful or failed delivery attempts.
- **Database Migrations**: Simple and versioned database schema migration using [Goose](https://github.com/pressly/goose).

---

## Architecture

Vessel consists of two primary services sharing a PostgreSQL database and a Redis task broker:

```mermaid
flowchart TD
    Client["Client App"] -->|1. POST /v1/send| API["API Server (Fiber)"]
    API -->|2. Insert 'queued' log| DB[("PostgreSQL")]
    API -->|3. Enqueue delivery task| Broker[("Redis/Asynq")]
    API -->|4. Return 202 Accepted| Client
    
    Worker["Worker Service"] -->|5. Dequeue task| Broker
    Worker -->|6. Fetch log & route| DB
    Worker -->|7. Decrypt credentials| Crypto["AES-256-GCM"]
    Worker -->|8. Send email| MailProvider["SMTP / Resend API"]
    Worker -->|9. Update status 'sent'/'failed'| DB
```

---

## Technical Stack

- **Runtime**: Go 1.24.0+
- **HTTP Framework**: [Fiber v2](https://gofiber.io/)
- **Task Queue**: [Asynq](https://github.com/hibiken/asynq) + Redis
- **Database**: PostgreSQL (Driver: `lib/pq`)
- **Database Migrations**: [Goose](https://github.com/pressly/goose)

---

## Getting Started

### Prerequisites

- Go 1.24 or later
- Docker and Docker Compose

### 1. Start Infrastructure Services

Use the `Makefile` command to boot up PostgreSQL and Redis in the background:

```bash
make up
```

### 2. Run Database Migrations

Apply database migrations to set up the schema:

```bash
make migrate
```

### 3. Run the Services

You can run the API server and the background worker concurrently:

**Start the API Server**:
```bash
go run cmd/api/main.go
```

**Start the Background Worker**:
```bash
go run cmd/worker/main.go
```

### Running Tests

Execute the integration test suite:

```bash
make test
```

---

## Environment Configuration

The services require the following environment variables (defined with defaults in development):

| Env Variable | Description | Example / Default |
| :--- | :--- | :--- |
| `DATABASE_URL` | PostgreSQL connection string | `postgres://vessel:vessel@127.0.0.1:5432/vessel?sslmode=disable` |
| `REDIS_ADDR` | Redis instance address for Asynq | `127.0.0.1:6379` |
| `API_PORT` | Port the API server listens on | `:3000` |
| `MASTER_ENCRYPTION_KEY` | 32-byte key for AES-256 credential encryption | `12345678901234567890123456789012` |

---

## API Endpoints

### 1. Send Single Email

Send a transactional email using the default configured route.

- **URL**: `/v1/send`
- **Method**: `POST`
- **Headers**: `Content-Type: application/json`
- **Payload**:

```json
{
  "recipient": "recipient@example.com",
  "subject": "Welcome to Vessel!",
  "body_html": "<p>Hello! Your account is ready.</p>"
}
```

- **Response (`202 Accepted`)**:

```json
{
  "id": "e30b1dbf-7f72-4d2d-9475-7b56f8f53a81",
  "status": "queued"
}
```

### 2. Send Batch Personalised Emails (Test Only)

Send multiple personalized emails in a single request.

- **URL**: `/v1/send/batch`
- **Method**: `POST`
- **Headers**: `Content-Type: application/json`
- **Payload**:

```json
{
  "emails": [
    {
      "recipient": "alice@example.com",
      "subject": "Hello {{name}}",
      "body_html": "<p>Welcome, {{name}}!</p>",
      "variables": { "name": "Alice" }
    },
    {
      "recipient": "bob@example.com",
      "subject": "Hello {{name}}",
      "body_html": "<p>Welcome, {{name}}!</p>",
      "variables": { "name": "Bob" }
    }
  ]
}
```

- **Response (`202 Accepted`)**:

```json
{
  "ids": [
    "4d2091fb-55f6-455b-b9d9-0c6d59b21f92",
    "9d18e957-c875-40e1-bbcb-875f6bc9a752"
  ],
  "status": "queued",
  "count": 2
}
```
