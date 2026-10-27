package app

import (
	"context"
	"time"

	"blotato/internal/domain"
)

// Repository methods define atomic use cases. Implementations must persist both
// state changes and newly scheduled actions in the same transaction.
type Repository interface {
	CreateAutomation(context.Context, string, domain.AutomationInput) (domain.Automation, error)
	ListAutomations(context.Context, string, domain.Page) ([]domain.Automation, error)
	GetAutomation(context.Context, string, string) (domain.Automation, error)
	SetEnabled(context.Context, string, string, bool) (domain.Automation, error)
	Ingest(context.Context, string, string, domain.Event) (domain.EventResult, error)
	ListExecutions(context.Context, string, domain.Page) ([]domain.Execution, error)
	GetExecution(context.Context, string, string) (domain.Execution, error)
	Claim(context.Context, time.Duration) (*domain.Action, error)
	Delivered(context.Context, domain.Action, time.Duration) (bool, error)
	Retry(context.Context, domain.Action, string, time.Duration, bool) (bool, error)
	Expire(context.Context) (int64, error)
	Ping(context.Context) error
}

type Platform interface {
	// Send must use action.ID as its stable idempotency key where supported.
	Send(context.Context, domain.Action) error
}

type Simulator interface {
	ListOutbound(context.Context, string, domain.Page) ([]domain.Outbound, error)
	SetFailure(context.Context, string, domain.FailurePlan) error
}

// DeliveryError carries a safe error class, never provider response bodies.
type DeliveryError struct {
	Code      string
	Permanent bool
}

func (e *DeliveryError) Error() string { return e.Code }
