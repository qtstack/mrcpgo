// Command mrcpgo runs a Go MRCPv2 media resource server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/qtstack/mrcpgo/internal/config"
	"github.com/qtstack/mrcpgo/internal/engine/mock"
	"github.com/qtstack/mrcpgo/internal/server"
)

// version is the build version reported at startup.
const version = "0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mrcpgo:", err)
		os.Exit(1)
	}
}

func run() error {
	var configPath string
	flag.StringVar(&configPath, "config", "", "path to the YAML configuration file")
	flag.Parse()

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	log, err := cfg.NewLogger()
	if err != nil {
		return err
	}
	slog.SetDefault(log)

	engines := mock.NewFactory()
	engines.RecognitionDelay = cfg.MRCP.Engine.RecognitionDelay.Value()
	engines.FramesPerUtterance = cfg.MRCP.Engine.FramesPerUtterance

	srv, err := server.New(cfg, log, engines)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.HTTP.Listen != "" {
		go serveHealth(ctx, cfg.HTTP.Listen, srv, log)
	}

	log.Info("mrcpgo starting", "version", version, "sip_listen", cfg.SIP.Listen)
	if err := srv.Run(ctx); err != nil {
		return err
	}
	log.Info("mrcpgo stopped")
	return nil
}

// serveHealth exposes /healthz on the configured address. The endpoint is
// optional and purely informational.
func serveHealth(ctx context.Context, addr string, srv *server.Server, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "ok\nsessions=%d\n", srv.Sessions().Len())
	})

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Debug("health endpoint shutdown", "error", err)
		}
	}()

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Warn("health endpoint stopped", "error", err)
	}
}
