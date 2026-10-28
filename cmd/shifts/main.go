// Команда shifts запускает HTTP-сервис смен курьеров.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq" // драйвер PostgreSQL для database/sql

	"github.com/Mashna33/go-courier-shifts/internal/api"
	"github.com/Mashna33/go-courier-shifts/internal/config"
	"github.com/Mashna33/go-courier-shifts/internal/postgres"
	"github.com/Mashna33/go-courier-shifts/internal/shifts"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	// Остановка по Ctrl+C или SIGTERM (так останавливает контейнер Docker).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openDB(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := postgres.Migrate(ctx, db); err != nil {
		return err
	}
	log.Info("migrations applied")

	svc := shifts.NewService(postgres.NewRepo(db), time.Now)
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.New(svc, db.PingContext, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("http server started", "addr", cfg.HTTPAddr)
		serverErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Даём текущим запросам время завершиться.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// openDB открывает пул соединений и ждёт, пока база станет доступна:
// в docker compose сервис может стартовать раньше PostgreSQL.
func openDB(ctx context.Context, url string, log *slog.Logger) (*sql.DB, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	deadline := time.Now().Add(30 * time.Second)
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := db.PingContext(pingCtx)
		cancel()
		if err == nil {
			return db, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			_ = db.Close()
			return nil, fmt.Errorf("database is not reachable: %w", err)
		}
		log.Warn("waiting for database", "err", err)
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			_ = db.Close()
			return nil, ctx.Err()
		}
	}
}
