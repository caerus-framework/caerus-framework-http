// Command stdlib demonstrates the golden-path wiring for cf_http: the HTTP
// server is declared as chassis in main, and an app component owns the router,
// middleware, and handler installation.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	cf "github.com/caerus-framework/caerus-framework"
	cf_http "github.com/caerus-framework/caerus-framework-http"
)

// app is the product component. It owns the routes and installs the final
// handler on the HTTP peer at Init.
type app struct {
	name    string
	httpSrv *cf_http.Server
}

func newApp() *app {
	return &app{name: "stdlib"}
}

func (a *app) Name() string                { return a.name }
func (a *app) GetInitOrderStage() cf.Stage { return cf_http.ComponentStage }

// GetDependencies declares the HTTP peer so the framework enforces it.
func (a *app) GetDependencies() []string {
	return []string{cf_http.ComponentName}
}

// Init resolves the HTTP peer, builds the router and middleware chain, and
// installs the handler. The server's own logger (resolved from the logs
// component) feeds RequestLog and Recover.
func (a *app) Init(ctx context.Context, fw *cf.CaerusFramework) error {
	srv, ok := cf.Get[*cf_http.Server](fw)
	if !ok {
		return errors.New("http server not registered")
	}
	a.httpSrv = srv

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Hello, World!"))
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	handler := cf_http.Chain(
		cf_http.Metrics(srv),
		cf_http.RequestID(),
		cf_http.RequestLog(srv.Logger),
		cf_http.Recover(srv.Logger, nil),
	)(mux)

	srv.SetHandler(handler)
	return nil
}

// Shutdown is a no-op for this app; the framework drains the HTTP server.
func (a *app) Shutdown(ctx context.Context) error { return nil }

func main() {
	httpServer := cf_http.New(
		cf_http.WithBind(":8080"),
		cf_http.WithConfigSource("http", "config/http.json"),
	)

	fw := cf.New(&cf.FrameworkOptions{
		Logs: &cf.LogsSettings{
			Format:       "json",
			Level:        "info",
			ConfigSource: "logs",
		},
		Observability: &cf.ObservabilitySettings{
			ConfigSource: "observability",
		},
		Components: []cf.CaerusComponent{
			httpServer,
			newApp(),
		},
	})

	// RunWithSignals initializes, absorbs argv (flags, config paths, jobs),
	// runs until SIGINT/SIGTERM, and drains with the given timeouts.
	if err := fw.RunWithSignals(context.Background(),
		cf.WithInitTimeout(10*time.Second),
		cf.WithShutdownTimeout(15*time.Second),
	); err != nil {
		fmt.Fprintf(os.Stderr, "run: %v\n", err)
		os.Exit(1)
	}
}
