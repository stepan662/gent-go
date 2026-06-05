package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
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
	pgDSN := flag.String("pg", "", "PostgreSQL DSN (e.g. postgres://user:pass@host/db). When set, --db is ignored.")
	httpAddr := flag.String("http", ":8448", "HTTP listen address (empty to disable)")
	tcpAddr := flag.String("tcp", "", "TCP listen address, e.g. 127.0.0.1:9090 (empty to disable)")
	udsPath := flag.String("uds", "", "Unix socket path, e.g. /tmp/gent.sock (empty to disable)")
	pollMs := flag.Int("poll", 500, "Engine poll interval in milliseconds")
	maxConcurrent := flag.Int("max-concurrent", 100_000, "Max instances processed concurrently")
	pprofAddr := flag.String("pprof", "", "pprof listen address, e.g. localhost:6060 (empty to disable)")
	logLevel := flag.String("log", "debug", "Log level: debug, info, warn, error")
	flag.Parse()

	log := newLogger(*logLevel)

	var database *db.DB
	var dbErr error
	if *pgDSN != "" {
		database, dbErr = db.OpenPostgres(*pgDSN)
		log.Info("database opened", "driver", "postgres")
	} else {
		database, dbErr = db.OpenSQLite(*dbPath)
		log.Info("database opened", "driver", "sqlite", "path", *dbPath)
	}
	if dbErr != nil {
		log.Error("open database", "err", dbErr)
		os.Exit(1)
	}
	defer database.Close()

	eng := engine.New(database, time.Duration(*pollMs)*time.Millisecond, *maxConcurrent, log)
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

	if *pprofAddr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Info("pprof listening", "addr", *pprofAddr)
			if err := http.ListenAndServe(*pprofAddr, nil); err != nil {
				log.Error("pprof server", "err", err)
			}
		}()
	}

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
			if err := srv.ListenTCP(ctx, *tcpAddr); err != nil {
				log.Error("TCP server", "err", err)
			}
		}()
	}

	if *udsPath != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.ListenUDS(ctx, *udsPath); err != nil {
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
