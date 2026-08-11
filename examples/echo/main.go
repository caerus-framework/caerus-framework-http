// Command echo demonstrates wiring an Echo server behind cf_http with the
// golden-path pattern. Echo's route patterns are reported via a small Record
// shim; the framework's generic middleware still wraps the router.
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
	"github.com/labstack/echo/v4"
)

// app is the product component. It owns the Echo router and installs it as the
// HTTP peer's handler at Init.
type app struct {
	name    string
	httpSrv *cf_http.Server
}

func newApp() *app {
	return &app{name: "echo"}
}

func (a *app) Name() string                { return a.name }
func (a *app) GetInitOrderStage() cf.Stage { return cf_http.ComponentStage }

// GetDependencies declares the HTTP peer so the framework enforces it.
func (a *app) GetDependencies() []string {
	return []string{cf_http.ComponentName}
}

// Init resolves the HTTP peer, builds the Echo router, and installs it.
func (a *app) Init(ctx context.Context, fw *cf.CaerusFramework) error {
	srv, ok := cf.Get[*cf_http.Server](fw)
	if !ok {
		return errors.New("http server not registered")
	}
	a.httpSrv = srv

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	e.GET("/", func(c echo.Context) error {
		return c.String(http.StatusOK, "Hello, World!")
	})
	e.GET("/health", func(c echo.Context) error {
		return c.String(http.StatusOK, "OK")
	})
	e.GET("/users/:id", func(c echo.Context) error {
		return c.String(http.StatusOK, "user "+c.Param("id"))
	})

	// Echo middleware order: first Use is outermost. The Record shim is
	// outermost so every request gets a route pattern label.
	e.Use(echoMetrics(srv))
	e.Use(echo.WrapMiddleware(cf_http.RequestID()))
	e.Use(echo.WrapMiddleware(cf_http.RequestLog(srv.Logger)))
	e.Use(echo.WrapMiddleware(cf_http.Recover(srv.Logger, nil)))

	srv.SetHandler(e)
	return nil
}

// Shutdown is a no-op for this app; the framework drains the HTTP server.
func (a *app) Shutdown(ctx context.Context) error { return nil }

// echoMetrics records Echo requests with route patterns. c.Path() returns the
// registered pattern (e.g. /users/:id), never a raw path with unbounded
// cardinality. Echo writes error statuses through its error handler after the
// middleware chain, so a returned error is translated here.
func echoMetrics(server *cf_http.Server) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()
			err := next(c)
			status := c.Response().Status
			if err != nil {
				if he, ok := err.(*echo.HTTPError); ok {
					status = he.Code
				} else {
					status = http.StatusInternalServerError
				}
			}
			cf_http.Record(server, c.Path(), status, time.Since(start))
			return err
		}
	}
}

func main() {
	httpServer := cf_http.New(
		cf_http.WithAddress(":8080"),
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
