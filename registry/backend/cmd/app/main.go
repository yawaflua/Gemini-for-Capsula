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

	"github.com/yawaflua/marusya-gemini/registry/backend/internal/config"
	"github.com/yawaflua/marusya-gemini/registry/backend/internal/httpapi"
)

func main() {
	flag.Parse()

	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)

	provider := config.Provider{}
	service := provider.RegistryServer(ctx)
	httpAddress := fmt.Sprint(provider.C().HTTP.Host, ":", provider.C().HTTP.Port)
	httpServer := &http.Server{
		Addr:              httpAddress,
		Handler:           httpapi.NewHandler(service, provider.Authenticator(ctx), provider.C().HTTP.SiteDir),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("HTTP server started", "address", httpAddress, "swagger", "/swagger/")
		if serveErr := httpServer.ListenAndServe(); !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
	}()

	var err error
	select {
	case <-ctx.Done():
	case err = <-errCh:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if shutdownErr := httpServer.Shutdown(shutdownCtx); err == nil {
		err = shutdownErr
	}

	return err
}
