package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

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
		ReadTimeout:  5 * 1000, // 5 s
		WriteTimeout: 10 * 1000, // 10 s
	})

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
