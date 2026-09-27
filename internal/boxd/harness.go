package boxd

import (
	"context"
	"time"
)

// ConvSpec identifies the persistent conversation to start or resume.
type ConvSpec struct {
	ConversationID string
	Provider       string
	Model          string
	SessionID      string
}

// Harness starts provider sessions.
type Harness interface {
	Start(context.Context, ConvSpec) (Session, error)
}

// Session is a live provider conversation.
type Session interface {
	Send(prompt string) error
	Steer(message string) error
	Interrupt() error
	Events() <-chan HarnessEvent
	SessionID() string
	Close() error
}

// HarnessEvent is a normalized provider event.
type HarnessEvent struct {
	Type      string         `json:"type"`
	Data      map[string]any `json:"data,omitempty"`
	SessionID string         `json:"sessionId,omitempty"`
	Time      time.Time      `json:"time,omitempty"`
}
