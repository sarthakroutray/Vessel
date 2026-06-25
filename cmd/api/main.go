package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/hibiken/asynq"
	"github.com/sarthak/vessel/internal/api"
	"github.com/sarthak/vessel/internal/config"
	"github.com/sarthak/vessel/internal/db"
)

func main() {
	// --- Configuration -----------------------------------------------------------
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config.Load: %v", err)
	}

	// --- Database ----------------------------------------------------------------
	sqlDB, err := db.Connect(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("db.Connect: %v", err)
	}
	defer sqlDB.Close()

	// --- Redis (asynq client for enqueuing tasks) --------------------------------
	redisClient := asynq.NewClient(asynq.RedisClientOpt{Addr: cfg.RedisAddr})
	defer redisClient.Close()

	// --- HTTP router -------------------------------------------------------------
	app := fiber.New(fiber.Config{
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		// BodyLimit caps Fiber's own body parser at the same ceiling enforced by
		// the RequestSizeLimit middleware, providing defence-in-depth.
		BodyLimit: api.MaxRequestBodyBytes,
	})

	// Enforce a hard 25 MB ceiling on every route before any handler runs.
	app.Use(api.RequestSizeLimit())

	// --- Routes ------------------------------------------------------------------
	app.Post("/v1/send", api.SendEmailHandler(sqlDB, redisClient))

	// --- Graceful shutdown -------------------------------------------------------
	// Listen in a goroutine so we can catch OS signals.
	go func() {
		log.Printf("API server listening on %s", cfg.APIPort)
		if err := app.Listen(cfg.APIPort); err != nil {
			log.Fatalf("app.Listen: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("shutting down server…")
	if err := app.Shutdown(); err != nil {
		log.Fatalf("app.Shutdown: %v", err)
	}
}
