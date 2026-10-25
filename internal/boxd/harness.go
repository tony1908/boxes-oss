package boxd

import (
	"context"
	"regexp"
	"strings"
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

// harnessErrorMaxChars caps error text surfaced to callers of a turn.
const harnessErrorMaxChars = 500

// harnessErrorText turns a provider error into the text reported on turn.failed:
// "name: message", with key-looking tokens redacted and the result capped at
// harnessErrorMaxChars. The fallback is used only when no detail exists.
func harnessErrorText(name, message, fallback string) string {
	name = strings.TrimSpace(name)
	message = strings.TrimSpace(message)
	if message == "" && name == "" {
		return fallback
	}
	text := name
	if message != "" {
		message = redactKeyLikeTokens(message)
		if text != "" {
			text += ": "
		}
		text += message
	}
	if runes := []rune(text); len(runes) > harnessErrorMaxChars {
		text = string(runes[:harnessErrorMaxChars])
	}
	return text
}

var (
	skKeyPattern         = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)
	longTokenPattern     = regexp.MustCompile(`[A-Za-z0-9+/_=-]{24,}`)
	redactionPlaceholder = "[redacted]"
)

// redactKeyLikeTokens masks API-key-looking substrings: sk-... keys and long
// tokens of 24+ characters that contain a digit (hex/base64-like). Pure-letter
// identifiers such as error names are left alone.
func redactKeyLikeTokens(text string) string {
	text = skKeyPattern.ReplaceAllString(text, redactionPlaceholder)
	return longTokenPattern.ReplaceAllStringFunc(text, func(token string) string {
		if strings.ContainsAny(token, "0123456789") {
			return redactionPlaceholder
		}
		return token
	})
}
