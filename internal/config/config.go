package config

import (
	"fmt"
	"os"
)

// AppConfig holds all environment-driven configuration for the service.
type AppConfig struct {
	DatabaseURL          string
	RedisAddr            string
	APIPort              string
	MasterEncryptionKey  string
}

// mustGetEnv panics if the variable is empty. Use for required fields.
func mustGetEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic(fmt.Sprintf("required environment variable %q is not set", key))
	}
	return v
}

// Load reads the environment and returns a validated AppConfig.
func Load() (*AppConfig, error) {
	cfg := &AppConfig{
		DatabaseURL:         mustGetEnv("DATABASE_URL"),
		RedisAddr:           mustGetEnv("REDIS_ADDR"),
		APIPort:             mustGetEnv("API_PORT"),
		MasterEncryptionKey: mustGetEnv("MASTER_ENCRYPTION_KEY"),
	}

	// Quick sanity: an AES-256 key must be exactly 32 bytes.
	if len(cfg.MasterEncryptionKey) != 32 {
		return nil, fmt.Errorf("MASTER_ENCRYPTION_KEY must be exactly 32 bytes (got %d)", len(cfg.MasterEncryptionKey))
	}

	return cfg, nil
}
