package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/hibiken/asynq"
	"github.com/sarthak/vessel/internal/config"
	"github.com/sarthak/vessel/internal/db"
	"github.com/sarthak/vessel/internal/queue"
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

	// --- Asynq server ------------------------------------------------------------
	//
	// Concurrency: 10 — a sensible maximum for a modest VPS. Each goroutine may
	// hold an SMTP connection or make an HTTP call, so we don't want to blast the
	// VPS or Neon with uncontrolled parallelism.
	srv := asynq.NewServer(
		asynq.RedisClientOpt{Addr: cfg.RedisAddr},
		asynq.Config{
			Concurrency: 10,
		},
	)

	// --- Register handler --------------------------------------------------------
	handler := queue.NewHandler(sqlDB, []byte(cfg.MasterEncryptionKey))

	mux := asynq.NewServeMux()
	mux.HandleFunc("email:deliver", handler.HandleEmailDeliver)

	// --- Run --------------------------------------------------------------------
	go func() {
		log.Println("worker: listening for tasks via asynq…")
		if err := srv.Run(mux); err != nil {
			log.Fatalf("asynq.Run: %v", err)
		}
	}()

	// --- Graceful shutdown -------------------------------------------------------
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("worker: shutting down (waiting for active tasks to finish)…")
	srv.Shutdown()

	log.Println("worker: stopped")
}
