package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"blotato/internal/domain"
)

type WorkerRepository interface {
	Claim(context.Context, time.Duration) (*domain.Action, error)
	Delivered(context.Context, domain.Action, time.Duration) (bool, error)
	Retry(context.Context, domain.Action, string, time.Duration, bool) (bool, error)
	Expire(context.Context) (int64, error)
}

type WorkerConfig struct {
	LeaseDuration time.Duration
	SendTimeout   time.Duration
	PollInterval  time.Duration
	WaitTTL       time.Duration
	MaxAttempts   int
	BaseBackoff   time.Duration
	MaxBackoff    time.Duration
}

func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{LeaseDuration: 15 * time.Second, SendTimeout: 5 * time.Second, PollInterval: 100 * time.Millisecond, WaitTTL: 24 * time.Hour, MaxAttempts: 5, BaseBackoff: 250 * time.Millisecond, MaxBackoff: 30 * time.Second}
}

type Worker struct {
	repo     WorkerRepository
	platform Platform
	cfg      WorkerConfig
	logger   *slog.Logger
}

func NewWorker(repo WorkerRepository, platform Platform, cfg WorkerConfig, logger *slog.Logger) (*Worker, error) {
	if repo == nil || platform == nil {
		return nil, fmt.Errorf("worker requires repository and platform")
	}
	if cfg.SendTimeout <= 0 || cfg.LeaseDuration <= 2*cfg.SendTimeout || cfg.PollInterval <= 0 || cfg.WaitTTL <= 0 || cfg.WaitTTL > 365*24*time.Hour || cfg.MaxAttempts < 1 || cfg.MaxAttempts > 20 || cfg.BaseBackoff <= 0 || cfg.MaxBackoff < cfg.BaseBackoff || cfg.MaxBackoff > time.Hour {
		return nil, fmt.Errorf("invalid worker timing or attempt configuration")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{repo: repo, platform: platform, cfg: cfg, logger: logger}, nil
}

// WorkOnce commits a claim before calling the adapter. All durable changes use
// the claim token. A canceled process leaves recoverable leased work behind.
func (w *Worker) WorkOnce(ctx context.Context) (bool, error) {
	a, err := w.repo.Claim(ctx, w.cfg.LeaseDuration)
	if err != nil {
		return false, err
	}
	if a == nil {
		return false, nil
	}
	log := w.logger.With("execution_id", a.ExecutionID, "action_id", a.ID, "kind", a.Kind, "attempt", a.Attempts)
	// A crash can consume the final allowed attempt without recording failure.
	// Reclaim that row and terminalize it; never strand it with attempts=max.
	if a.Attempts > w.cfg.MaxAttempts {
		ok, err := w.repo.Retry(ctx, *a, "attempts_exhausted", 0, true)
		if err == nil && ok {
			log.WarnContext(ctx, "action exhausted after lease recovery")
		}
		return true, err
	}
	log.InfoContext(ctx, "action attempt")
	sendCtx, cancel := context.WithTimeout(ctx, w.cfg.SendTimeout)
	err = w.platform.Send(sendCtx, *a)
	cancel()
	if err == nil {
		ok, ackErr := w.repo.Delivered(ctx, *a, w.cfg.WaitTTL)
		if ackErr == nil {
			log.InfoContext(ctx, "action acknowledgment", "accepted", ok)
		}
		return true, ackErr
	}
	code := "temporary_delivery_failure"
	permanent := false
	var delivery *DeliveryError
	if errors.As(err, &delivery) && delivery.Permanent {
		permanent = true
		code = "permanent_delivery_failure"
	}
	if a.Attempts >= w.cfg.MaxAttempts {
		permanent = true
		code = "attempts_exhausted"
	}
	accepted, retryErr := w.repo.Retry(ctx, *a, code, w.backoff(a.Attempts), permanent)
	if retryErr == nil {
		log.WarnContext(ctx, "action delivery failed", "error_class", code, "terminal", permanent, "accepted", accepted)
	}
	return true, retryErr
}

func (w *Worker) backoff(attempt int) time.Duration {
	d := w.cfg.BaseBackoff
	for i := 1; i < attempt && d < w.cfg.MaxBackoff; i++ {
		if d > w.cfg.MaxBackoff/2 {
			d = w.cfg.MaxBackoff
			break
		}
		d *= 2
	}
	if d > w.cfg.MaxBackoff {
		d = w.cfg.MaxBackoff
	}
	return d/2 + time.Duration(rand.Int64N(int64(d-d/2)+1))
}

func (w *Worker) Run(ctx context.Context) error {
	poll := time.NewTicker(w.cfg.PollInterval)
	defer poll.Stop()
	expiry := time.NewTicker(time.Second)
	defer expiry.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-expiry.C:
			n, err := w.repo.Expire(ctx)
			if err != nil && ctx.Err() == nil {
				w.logger.ErrorContext(ctx, "expiry sweep failed")
			}
			if n > 0 {
				w.logger.InfoContext(ctx, "waiting workflows expired", "count", n)
			}
		default:
		}
		worked, err := w.WorkOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.logger.ErrorContext(ctx, "worker database operation failed")
		}
		if worked && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
		}
	}
}
