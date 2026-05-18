package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// Server listens on HTTP, TCP, and/or Unix Domain Socket simultaneously.
// All three transports share the same JSON envelope protocol.
type Server struct {
	handlers *Handlers
	log      *slog.Logger
}

func NewServer(handlers *Handlers, log *slog.Logger) *Server {
	return &Server{handlers: handlers, log: log}
}

// ListenHTTP starts an HTTP server on addr and shuts it down gracefully when ctx is cancelled.
//
//	POST /            — send any action (same envelope protocol as TCP/UDS)
//	GET  /docs        — Swagger UI
//	GET  /openapi.json — OpenAPI 3.0 spec
func (s *Server) ListenHTTP(ctx context.Context, addr string) error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, swaggerUI)
	})

	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildSpec())
	})

	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		var env Envelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			writeReply(w, errReply(fmt.Errorf("decode envelope: %w", err)))
			return
		}
		writeReply(w, s.handlers.Handle(env))
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	s.log.Info("HTTP listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// ListenTCP starts a JSON stream server on a TCP address (e.g. "127.0.0.1:9090").
func (s *Server) ListenTCP(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen tcp %s: %w", addr, err)
	}
	s.log.Info("TCP listening", "addr", addr)
	return s.acceptLoop(ln)
}

// ListenUDS starts a JSON stream server on a Unix Domain Socket path.
func (s *Server) ListenUDS(path string) error {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen uds %s: %w", path, err)
	}
	s.log.Info("UDS listening", "path", path)
	return s.acceptLoop(ln)
}

func (s *Server) acceptLoop(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if strings.Contains(err.Error(), "use of closed network connection") {
				return nil
			}
			s.log.Error("accept error", "err", err)
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var env Envelope
		if err := dec.Decode(&env); err != nil {
			return
		}
		if err := enc.Encode(s.handlers.Handle(env)); err != nil {
			s.log.Warn("write reply", "err", err)
			return
		}
	}
}

func writeReply(w http.ResponseWriter, r Reply) {
	w.Header().Set("Content-Type", "application/json")
	if !r.OK {
		w.WriteHeader(http.StatusBadRequest)
	}
	json.NewEncoder(w).Encode(r)
}
