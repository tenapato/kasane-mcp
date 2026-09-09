package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tenapato/kasane-mcp/internal/core"
	"github.com/tenapato/kasane-mcp/internal/search"
	"github.com/tenapato/kasane-mcp/internal/server"
	"github.com/tenapato/kasane-mcp/internal/store"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func main() {
	if e := run(); e != nil {
		slog.Error("kasane stopped", "error", e)
		os.Exit(1)
	}
}
func run() error {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}
	if cmd == "healthcheck" {
		c := http.Client{Timeout: 3 * time.Second}
		addr := env("LISTEN_ADDR", ":8080")
		if strings.HasPrefix(addr, ":") {
			addr = "127.0.0.1" + addr
		}
		r, e := c.Get("http://" + addr + "/healthz")
		if e != nil {
			return e
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			return fmt.Errorf("health check failed: %d", r.StatusCode)
		}
		return nil
	}
	if cmd != "serve" && cmd != "migrate" && cmd != "bootstrap" && cmd != "reindex" {
		return fmt.Errorf("usage: kasane [serve|migrate|bootstrap --username NAME|reindex --workspace UUID|healthcheck]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	connect, cancel := context.WithTimeout(ctx, 15*time.Second)
	s, e := store.Open(connect, dbURL)
	cancel()
	if e != nil {
		return fmt.Errorf("cannot connect to PostgreSQL (check DATABASE_URL and database readiness)")
	}
	defer s.Close()
	if e = s.Migrate(ctx); e != nil {
		return fmt.Errorf("database migration failed: %w", e)
	}
	switch cmd {
	case "migrate":
		fmt.Println("Database schema ready.")
		return nil
	case "bootstrap":
		f := flag.NewFlagSet(cmd, flag.ContinueOnError)
		username := f.String("username", "owner", "Owner username")
		if e = f.Parse(args); e != nil {
			return e
		}
		password := os.Getenv("KASANE_ADMIN_PASSWORD")
		if file := os.Getenv("KASANE_ADMIN_PASSWORD_FILE"); file != "" {
			b, e := os.ReadFile(file)
			if e != nil {
				return e
			}
			password = strings.TrimRight(string(b), "\r\n")
		}
		if e = server.Bootstrap(ctx, s, *username, password); e != nil {
			return e
		}
		fmt.Println("Owner created.")
		return nil
	case "reindex":
		f := flag.NewFlagSet(cmd, flag.ContinueOnError)
		ws := f.String("workspace", "", "Workspace UUID, omitted for all")
		if e = f.Parse(args); e != nil {
			return e
		}
		if *ws != "" && !core.ValidID(*ws) {
			return fmt.Errorf("invalid workspace ID")
		}
		if e = s.Reindex(ctx, *ws); e != nil {
			return e
		}
		fmt.Println("Index rebuild queued. Run the server to process it.")
		return nil
	}
	q := search.NewQdrant(env("QDRANT_URL", "http://localhost:6333"), os.Getenv("QDRANT_API_KEY"))
	app, e := server.New(s, q, server.Config{PublicURL: env("PUBLIC_URL", "http://localhost:8080"), WebDir: os.Getenv("WEB_DIR"), TrustedProxyCIDRs: os.Getenv("TRUSTED_PROXY_CIDRS")})
	if e != nil {
		return e
	}
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); app.RunWorker(ctx) }()
	srv := http.Server{Addr: env("LISTEN_ADDR", ":8080"), Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	done := make(chan error, 1)
	go func() { slog.Info("kasane listening", "address", srv.Addr); done <- srv.ListenAndServe() }()
	select {
	case e = <-done:
		stop()
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	<-workerDone
	if e != nil && e != http.ErrServerClosed {
		return e
	}
	return nil
}
