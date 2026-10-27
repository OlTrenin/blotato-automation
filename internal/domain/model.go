package domain

import (
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("identity reused with different content")
	ErrInvalid  = errors.New("invalid input")
	ErrQuota    = errors.New("tenant storage quota exceeded")
)

type Automation struct {
	ID           string    `json:"id"`
	TenantID     string    `json:"-"`
	AccountID    string    `json:"account_id"`
	Platform     string    `json:"platform"`
	Keyword      string    `json:"keyword"`
	CommentReply string    `json:"comment_reply"`
	EmailPrompt  string    `json:"email_prompt"`
	Link         string    `json:"link"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
}

type AutomationInput struct {
	AccountID    string `json:"account_id"`
	Platform     string `json:"platform"`
	Keyword      string `json:"keyword"`
	CommentReply string `json:"comment_reply"`
	EmailPrompt  string `json:"email_prompt"`
	Link         string `json:"link"`
}

type Event struct {
	EventID   string `json:"event_id"`
	Platform  string `json:"platform"`
	AccountID string `json:"account_id"`
	PostID    string `json:"post_id,omitempty"`
	CommentID string `json:"comment_id,omitempty"`
	UserID    string `json:"user_id"`
	MessageID string `json:"message_id,omitempty"`
	Text      string `json:"text"`
}

type EventResult struct {
	Outcome     string `json:"outcome"`
	ExecutionID string `json:"execution_id,omitempty"`
	Duplicate   bool   `json:"duplicate"`
}

type State string

const (
	InitialPending State = "INITIAL_ACTIONS_PENDING"
	WaitingEmail   State = "WAITING_FOR_EMAIL"
	FinalPending   State = "FINAL_MESSAGE_PENDING"
	Completed      State = "COMPLETED"
	Failed         State = "FAILED"
	Expired        State = "EXPIRED"
)

type ActionKind string

const (
	ReplyComment ActionKind = "reply_comment"
	RequestEmail ActionKind = "request_email"
	SendLink     ActionKind = "send_link"
)

type Execution struct {
	ID            string     `json:"id"`
	TenantID      string     `json:"-"`
	AutomationID  string     `json:"automation_id"`
	AccountID     string     `json:"account_id"`
	Platform      string     `json:"platform"`
	UserID        string     `json:"user_id"`
	CommentID     string     `json:"comment_id"`
	State         State      `json:"state"`
	Snapshot      Automation `json:"snapshot"`
	EmailReceived bool       `json:"email_received"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	Failure       string     `json:"failure,omitempty"`
	Actions       []Action   `json:"actions"`
}

type Action struct {
	ID          string     `json:"id"`
	ExecutionID string     `json:"execution_id"`
	TenantID    string     `json:"-"`
	AccountID   string     `json:"account_id"`
	Platform    string     `json:"platform"`
	UserID      string     `json:"user_id"`
	CommentID   string     `json:"comment_id,omitempty"`
	Kind        ActionKind `json:"kind"`
	Body        string     `json:"body"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	LeaseToken  string     `json:"-"`
	AvailableAt time.Time  `json:"available_at"`
	LeaseUntil  *time.Time `json:"lease_until,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

type Outbound struct {
	ID          string     `json:"id"`
	ExecutionID string     `json:"execution_id"`
	AccountID   string     `json:"account_id"`
	UserID      string     `json:"user_id"`
	CommentID   string     `json:"comment_id,omitempty"`
	Kind        ActionKind `json:"kind"`
	Body        string     `json:"body"`
	CreatedAt   time.Time  `json:"created_at"`
}

// FailurePlan is a development-only simulated adapter control.
type FailurePlan struct {
	Kind  ActionKind `json:"kind"`
	Mode  string     `json:"mode"` // transient, permanent, or after_success
	Count int        `json:"count"`
}
