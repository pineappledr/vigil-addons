package hub

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/pineappledr/vigil-addons/snapraid/internal/config"
)

type Server struct {
	cfg    *config.HubConfig
	mux    *http.ServeMux
	server *http.Server
	logger *slog.Logger
}

func NewServer(cfg *config.HubConfig, logger *slog.Logger) *Server {
	s := &Server{
		cfg:    cfg,
		mux:    http.NewServeMux(),
		logger: logger,
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status":"ok"}`)
}

func (s *Server) Start() error {
	addr := fmt.Sprintf(":%d", s.cfg.Listen.Port)
	s.server = &http.Server{
		Addr:    addr,
		Handler: s.mux,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("hub listen on %s: %w", addr, err)
	}

	s.logger.Info("hub server started", "addr", addr)
	return s.server.Serve(ln)
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("hub server shutting down")
	return s.server.Shutdown(ctx)
}
