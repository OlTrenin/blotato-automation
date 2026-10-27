package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"blotato/internal/app"
	"blotato/internal/domain"
	"blotato/internal/httpapi"
	"blotato/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

type config struct {
	databaseURL, token, tenantID, accountID, addr string
	demo                                          bool
	workers                                       int
	waitTTL                                       time.Duration
}

func loadConfig(getenv func(string) string) (config, error) {
	c := config{databaseURL: getenv("DATABASE_URL"), token: getenv("API_TOKEN"), tenantID: getenv("TENANT_ID"), accountID: getenv("ACCOUNT_ID"), addr: getenv("ADDR"), workers: 2, waitTTL: 24 * time.Hour}
	if c.databaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if len(c.token) < 16 || strings.ContainsAny(c.token, " \t\r\n") {
		return c, errors.New("API_TOKEN must contain at least 16 bytes without whitespace")
	}
	if c.tenantID == "" {
		c.tenantID = "tenant-demo"
	}
	if c.accountID == "" {
		c.accountID = "account-123"
	}
	if c.addr == "" {
		c.addr = "127.0.0.1:8080"
	}
	if !domain.ValidIdentifier(c.tenantID) || !domain.ValidIdentifier(c.accountID) {
		return c, errors.New("TENANT_ID and ACCOUNT_ID must be valid identifiers")
	}
	if value := getenv("DEMO_MODE"); value != "" {
		if value != "true" && value != "false" {
			return c, errors.New("DEMO_MODE must be true or false")
		}
		c.demo = value == "true"
	}
	if value := getenv("WORKERS"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 32 {
			return c, errors.New("WORKERS must be between 1 and 32")
		}
		c.workers = n
	}
	if value := getenv("WAIT_TTL"); value != "" {
		d, err := time.ParseDuration(value)
		if err != nil || d < time.Second || d > 30*24*time.Hour {
			return c, errors.New("WAIT_TTL must be between 1s and 720h")
		}
		c.waitTTL = d
	}
	return c, nil
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("service stopped", "reason", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	c, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	poolConfig, err := pgxpool.ParseConfig(c.databaseURL)
	if err != nil {
		return errors.New("invalid DATABASE_URL")
	}
	poolConfig.MaxConns = 20
	poolConfig.ConnConfig.ConnectTimeout = 5 * time.Second
	startup, cancelStartup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStartup()
	pool, err := pgxpool.NewWithConfig(startup, poolConfig)
	if err != nil {
		return errors.New("database pool initialization failed")
	}
	defer pool.Close()
	if err = pool.Ping(startup); err != nil {
		return errors.New("database connection failed")
	}
	if err = postgres.Migrate(startup, pool); err != nil {
		return errors.New("database migration failed")
	}
	repo := postgres.New(pool)
	if c.demo {
		if err = repo.Bootstrap(startup, c.tenantID, c.accountID); err != nil {
			return errors.New("demo account provisioning failed")
		}
	}
	wcfg := app.DefaultWorkerConfig()
	wcfg.WaitTTL = c.waitTTL
	worker, err := app.NewWorker(repo, repo, wcfg, log)
	if err != nil {
		return fmt.Errorf("worker configuration: %w", err)
	}
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	var workers sync.WaitGroup
	failures := make(chan error, 1)
	for i := 0; i < c.workers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := worker.Run(workerCtx); err != nil && workerCtx.Err() == nil {
				select {
				case failures <- errors.New("background worker stopped unexpectedly"):
				default:
				}
			}
		}()
	}
	srv := &http.Server{
		Addr:              c.addr,
		Handler:           httpapi.New(repo, repo, httpapi.Config{APIKeys: map[string]string{c.token: c.tenantID}, DemoMode: c.demo, Logger: log}),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case failures <- errors.New("HTTP listener failed"):
			default:
			}
		}
	}()
	log.Info("service started", "addr", c.addr, "workers", c.workers, "demo_mode", c.demo)
	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	select {
	case <-ctx.Done():
	case err = <-failures:
	}
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	if shutdownErr := srv.Shutdown(shutdown); shutdownErr != nil {
		_ = srv.Close()
		if err == nil {
			err = errors.New("HTTP graceful shutdown deadline exceeded")
		}
	}
	stopWorkers()
	workers.Wait()
	log.Info("service stopped gracefully")
	return err
}
