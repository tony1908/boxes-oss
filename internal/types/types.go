// Package types defines the shared domain and API types used by boxes.
package types

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"
)

const gibibyte = int64(1 << 30)

// Size contains the resource limits for a box size.
type Size struct {
	Name                   string
	CPUs                   int
	MemoryBytes            int64
	MemoryReservationBytes int64
	DiskBytes              int64
}

var sizes = map[string]Size{
	"xsmall":  {Name: "xsmall", CPUs: 1, MemoryBytes: 2 * gibibyte, MemoryReservationBytes: gibibyte / 8, DiskBytes: 12 * gibibyte},
	"small":   {Name: "small", CPUs: 2, MemoryBytes: 4 * gibibyte, MemoryReservationBytes: gibibyte / 4, DiskBytes: 12 * gibibyte},
	"default": {Name: "default", CPUs: 4, MemoryBytes: 8 * gibibyte, MemoryReservationBytes: gibibyte / 2, DiskBytes: 50 * gibibyte},
	"large":   {Name: "large", CPUs: 8, MemoryBytes: 16 * gibibyte, MemoryReservationBytes: gibibyte, DiskBytes: 125 * gibibyte},
	"xlarge":  {Name: "xlarge", CPUs: 16, MemoryBytes: 32 * gibibyte, MemoryReservationBytes: 2 * gibibyte, DiskBytes: 251 * gibibyte},
}

// SizeFor returns the resource limits for a named box size.
func SizeFor(name string) (Size, error) {
	size, ok := sizes[name]
	if !ok {
		return Size{}, fmt.Errorf("unknown box size %q", name)
	}
	return size, nil
}

// BoxState is the lifecycle state of a box.
type BoxState string

const (
	StateProvisioning BoxState = "provisioning"
	StateReady        BoxState = "ready"
	StateIdle         BoxState = "idle"
	StateStopping     BoxState = "stopping"
	StateStopped      BoxState = "stopped"
	StateResuming     BoxState = "resuming"
	StateError        BoxState = "error"
	StateDeleting     BoxState = "deleting"
	StateUnreachable  BoxState = "unreachable"
)

const idAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

var idEncoding = base32.NewEncoding(idAlphabet).WithPadding(base32.NoPadding)

// NewID creates an ID with the given prefix and 12 lowercase base32 characters.
func NewID(prefix string) string {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic(fmt.Sprintf("generate ID: %v", err))
	}
	return prefix + idEncoding.EncodeToString(random[:])[:12]
}

// Box describes a managed box.
type Box struct {
	ID                 string            `json:"id"`
	NodeID             string            `json:"nodeId"`
	Type               string            `json:"type"`
	State              BoxState          `json:"state"`
	Image              string            `json:"image"`
	EnvironmentID      string            `json:"environmentId,omitempty"`
	EnvironmentVersion int               `json:"environmentVersion,omitempty"`
	TTLSeconds         int64             `json:"ttlSeconds,omitempty"`
	IdleStopSeconds    int64             `json:"idleStopSeconds,omitempty"`
	ExpiresAt          *time.Time        `json:"expiresAt,omitempty"`
	FromSnapshot       string            `json:"fromSnapshot,omitempty"`
	KeepSnapshots      int               `json:"keepSnapshots,omitempty"`
	Tags               map[string]string `json:"tags,omitempty"`
	NetworkPolicy      *NetworkPolicy    `json:"networkPolicy,omitempty"`
	Source             *BoxSource        `json:"source,omitempty"`
	ErrorCode          string            `json:"errorCode,omitempty"`
	Error              string            `json:"error,omitempty"`
	CreatedAt          time.Time         `json:"createdAt"`
	StoppedAt          *time.Time        `json:"stoppedAt,omitempty"`
}

// BoxSource records where a sandbox's project files came from. It never holds credentials.
type BoxSource struct {
	Type     string `json:"type"`
	URL      string `json:"url"`
	Revision string `json:"revision,omitempty"`
	Path     string `json:"path"`
}

// MaxTags is the maximum number of tags a box may carry.
const MaxTags = 10

var tagKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

// ValidateTags checks tag count, key format, and value length and characters.
func ValidateTags(tags map[string]string) error {
	if len(tags) > MaxTags {
		return fmt.Errorf("tags must not exceed %d entries", MaxTags)
	}
	for key, value := range tags {
		if !tagKeyPattern.MatchString(key) {
			return fmt.Errorf("tag key %q must match %s", key, tagKeyPattern.String())
		}
		if utf8.RuneCountInString(value) > 255 {
			return fmt.Errorf("tag %q value must not exceed 255 characters", key)
		}
		for _, r := range value {
			if unicode.IsControl(r) {
				return fmt.Errorf("tag %q value must not contain control characters", key)
			}
		}
	}
	return nil
}

// Node describes a machine running a boxes agent.
type Node struct {
	ID                      string    `json:"id"`
	Name                    string    `json:"name"`
	Host                    string    `json:"host,omitempty"`
	HostBase                string    `json:"hostBase,omitempty"`
	Status                  string    `json:"status"`
	CPUs                    int       `json:"cpus"`
	MemoryBytes             int64     `json:"memoryBytes"`
	FreeMemoryBytes         int64     `json:"freeMemoryBytes"`
	MemAvailableBytes       int64     `json:"memAvailableBytes"`
	SwapFreeBytes           int64     `json:"swapFreeBytes"`
	MemoryPressureSomeAvg10 float64   `json:"memoryPressureSomeAvg10"`
	DiskFreeBytes           int64     `json:"diskFreeBytes"`
	LastSeen                time.Time `json:"lastSeen,omitempty"`
}

// Snapshot describes a restic snapshot associated with a box.
type Snapshot struct {
	ID            string    `json:"id"`
	BoxID         string    `json:"boxId"`
	ResticID      string    `json:"resticId"`
	SizeBytes     int64     `json:"sizeBytes"`
	CreatedAt     time.Time `json:"createdAt"`
	Repository    string    `json:"-"`
	StorageEnvEnc []byte    `json:"-"`
}

// NamedSnapshot pins a snapshot under a user-visible name.
type NamedSnapshot struct {
	Name       string    `json:"name"`
	SnapshotID string    `json:"snapshotId"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Event is an ordered event emitted by a box conversation or lifecycle.
type Event struct {
	Cursor         int64          `json:"cursor"`
	ID             string         `json:"id"`
	BoxID          string         `json:"boxId,omitempty"`
	ConversationID string         `json:"conversationId,omitempty"`
	RunID          string         `json:"runId,omitempty"`
	Type           string         `json:"type"`
	Time           time.Time      `json:"time"`
	Data           map[string]any `json:"data"`
}

// Conversation describes an agent conversation within a box.
type Conversation struct {
	ID         string    `json:"id"`
	BoxID      string    `json:"boxId,omitempty"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model,omitempty"`
	Prompts    int       `json:"prompts"`
	Running    bool      `json:"running"`
	LastPrompt string    `json:"lastPrompt,omitempty"`
	Current    bool      `json:"current"`
	CreatedAt  time.Time `json:"createdAt,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt,omitempty"`
}

// Run describes one prompt execution.
type Run struct {
	ID             string     `json:"id"`
	ConversationID string     `json:"conversationId"`
	Status         string     `json:"status"`
	Error          string     `json:"error,omitempty"`
	CreatedAt      time.Time  `json:"createdAt,omitempty"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	FinishedAt     *time.Time `json:"finishedAt,omitempty"`
}

// Environment is a named, versioned box configuration.
type Environment struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Version   int            `json:"version"`
	Document  map[string]any `json:"document,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt,omitempty"`
}

// Webhook describes a configured event destination.
type Webhook struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Events    []string  `json:"events"`
	Secret    string    `json:"-"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// APIKey describes a user API key. The raw key is only populated at creation.
type APIKey struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Key       string           `json:"key,omitempty"`
	Scopes    []string         `json:"scopes"`
	ExpiresAt *time.Time       `json:"expiresAt,omitempty"`
	CreatedAt time.Time        `json:"createdAt"`
	Usage     map[string]int64 `json:"usage,omitempty"`
}

// User describes an account.
type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	Role         string    `json:"role"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"createdAt"`
}

// APIError is a structured API error response payload.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e APIError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Message
}
