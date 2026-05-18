package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"gent/internal/api"
	"gent/internal/db"
	"gent/internal/engine"
)

func main() {
	dbPath := flag.String("db", "gent.db", "SQLite database file path")
	httpAddr := flag.String("http", ":8080", "HTTP listen address (empty to disable)")
	tcpAddr := flag.String("tcp", "", "TCP listen address, e.g. 127.0.0.1:9090 (empty to disable)")
	udsPath := flag.String("uds", "", "Unix socket path, e.g. /tmp/gent.sock (empty to disable)")
	pollMs := flag.Int("poll", 500, "Engine poll interval in milliseconds")
	logLevel := flag.String("log", "info", "Log level: debug, info, warn, error")
	flag.Parse()

	log := newLogger(*logLevel)

	database, err := db.Open(*dbPath)
	if err != nil {
		log.Error("open database", "err", err)
		os.Exit(1)
	}
	defer database.Close()
	log.Info("database opened", "path", *dbPath)

	eng := engine.New(database, time.Duration(*pollMs)*time.Millisecond, log)
	handlers := api.NewHandlers(database)
	srv := api.NewServer(handlers, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		eng.Run(ctx)
	}()

	if *httpAddr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.ListenHTTP(ctx, *httpAddr); err != nil {
				log.Error("HTTP server", "err", err)
			}
		}()
	}

	if *tcpAddr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.ListenTCP(*tcpAddr); err != nil {
				log.Error("TCP server", "err", err)
			}
		}()
	}

	if *udsPath != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.ListenUDS(*udsPath); err != nil {
				log.Error("UDS server", "err", err)
			}
		}()
	}

	<-ctx.Done()
	log.Info("shutting down")
	wg.Wait()
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
