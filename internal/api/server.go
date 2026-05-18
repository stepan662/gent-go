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

// Server listens on HTTP, TCP, and/or Unix Domain Socket simultaneously
// and routes all requests through Handlers.
type Server struct {
	handlers *Handlers
	log      *slog.Logger
}

func NewServer(handlers *Handlers, log *slog.Logger) *Server {
	return &Server{handlers: handlers, log: log}
}

// ListenHTTP starts an HTTP server on addr (e.g. ":8080") and shuts it down
// gracefully when ctx is cancelled.
//
// REST endpoints (canonical, documented in Swagger):
//
//	GET  /definitions        → list_definitions
//	PUT  /definitions        → put_definition
//	GET  /instances          → list_instances  (?status=running|completed|failed)
//	POST /instances          → start_instance
//	GET  /instances/{id}     → get_instance
//	GET  /docs               → Swagger UI
//	GET  /openapi.json       → OpenAPI 3.0 spec
//
// Envelope endpoint (also works for scripts / TCP / UDS parity):
//
//	POST /                   → any action via {"action":"...","payload":{...},"id":"..."}
func (s *Server) ListenHTTP(ctx context.Context, addr string) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, swaggerUI)
	})

	mux.HandleFunc("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(spec())
	})

	mux.HandleFunc("/definitions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeReply(w, s.handlers.listDefinitions())
		case http.MethodPut:
			var payload json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				writeReply(w, errReply(fmt.Errorf("decode body: %w", err)))
				return
			}
			writeReply(w, s.handlers.putDefinition(payload))
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/instances/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/instances/")
		if id == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeReply(w, s.handlers.getInstance(id))
	})

	mux.HandleFunc("/instances", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			status := r.URL.Query().Get("status")
			writeReply(w, s.handlers.listInstances(mustMarshal(map[string]string{"status": status})))
		case http.MethodPost:
			var payload json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				writeReply(w, errReply(fmt.Errorf("decode body: %w", err)))
				return
			}
			writeReply(w, s.handlers.startInstance(payload))
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
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

// acceptLoop accepts connections and handles each in a goroutine.
// Protocol: newline-delimited JSON Envelopes in, newline-delimited JSON Replies out.
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

func mustMarshal(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
