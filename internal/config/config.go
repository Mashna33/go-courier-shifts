// Package config читает настройки сервиса из переменных окружения.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

type Config struct {
	HTTPAddr    string
	DatabaseURL string
	LogLevel    slog.Level
}

// Load читает конфигурацию. DATABASE_URL обязателен, остальное имеет значения по умолчанию.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		HTTPAddr:    getenv("HTTP_ADDR"),
		DatabaseURL: getenv("DATABASE_URL"),
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":8080"
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}

	switch strings.ToLower(getenv("LOG_LEVEL")) {
	case "", "info":
		cfg.LogLevel = slog.LevelInfo
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "warn":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	default:
		return Config{}, fmt.Errorf("unknown LOG_LEVEL %q (use debug, info, warn or error)", getenv("LOG_LEVEL"))
	}
	return cfg, nil
}

// FromEnv — Load поверх реальных переменных окружения.
func FromEnv() (Config, error) { return Load(os.Getenv) }
