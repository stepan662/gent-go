package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// Server listens on HTTP, TCP, and/or Unix Domain Socket simultaneously.
// All three transports share the same handler logic; only the envelope extraction differs.
type Server struct {
	handlers *Handlers
	log      *slog.Logger
}

func NewServer(handlers *Handlers, log *slog.Logger) *Server {
	return &Server{handlers: handlers, log: log}
}

// ListenHTTP starts an HTTP server on addr and shuts it down gracefully when ctx is cancelled.
//
// Routes and Swagger docs are generated entirely from the action registry in actions.go.
// To add a new endpoint, add an entry there — no changes needed here or in openapi.go.
func (s *Server) ListenHTTP(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	h := s.handlers

	for _, a := range registry {
		a := a
		mux.HandleFunc(a.Method+" "+a.Path, func(w http.ResponseWriter, r *http.Request) {
			env, err := a.envelope(r)
			if err != nil {
				writeReply(w, errReply(fmt.Errorf("bad request: %w", err)))
				return
			}
			writeReply(w, a.handle(h, env))
		})
	}

	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, swaggerUIHTML("genroc API", "/openapi.json"))
	})

	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildSpec())
	})

	mux.HandleFunc("GET /process-schema.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildProcessDefinitionSchema())
	})

	mux.HandleFunc("GET /definitions/{name}/docs", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		specURL := "/definitions/" + name + "/openapi.json"
		if v := r.URL.Query().Get("version"); v != "" {
			specURL += "?version=" + v
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, swaggerUIHTML(name+" — genroc API", specURL))
	})

	mux.HandleFunc("GET /definitions/{name}/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		version := 0
		if v := r.URL.Query().Get("version"); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil {
				version = parsed
			}
		}
		data, err := h.ProcessSpec(r.PathValue("name"), version)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
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
// Protocol: newline-delimited JSON envelopes {"action":"...","payload":{...},"id":"..."}.
func (s *Server) ListenTCP(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen tcp %s: %w", addr, err)
	}
	s.log.Info("TCP listening", "addr", addr)
	return s.acceptLoop(ctx, ln)
}

// ListenUDS starts a JSON stream server on a Unix Domain Socket path.
func (s *Server) ListenUDS(ctx context.Context, path string) error {
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen uds %s: %w", path, err)
	}
	s.log.Info("UDS listening", "path", path)
	return s.acceptLoop(ctx, ln)
}

func (s *Server) acceptLoop(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
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
		json.NewEncoder(w).Encode(map[string]string{"error": r.Error})
	} else {
		json.NewEncoder(w).Encode(r.Data)
	}
}
