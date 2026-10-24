package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tony1908/boxes-oss/internal/diagnostics"
	"github.com/tony1908/boxes-oss/internal/store"
	"github.com/tony1908/boxes-oss/internal/types"
)

type pricingSettings struct {
	Currency     string             `json:"currency"`
	HourlyBySize map[string]float64 `json:"hourlyBySize"`
}

type activityCursor struct {
	time time.Time
	id   string
}

type activityItem struct {
	ID         string          `json:"id"`
	Time       time.Time       `json:"time"`
	ActorType  string          `json:"actorType"`
	ActorLabel string          `json:"actorLabel"`
	Category   string          `json:"category"`
	Action     string          `json:"action"`
	Detail     json.RawMessage `json:"detail"`
	DurationMS *int64          `json:"durationMs,omitempty"`
	Outcome    *string         `json:"outcome,omitempty"`
}

func (s *Server) pricing(ctx context.Context) pricingSettings { return s.readPricing(ctx) }

func (s *Server) runActivityPruner(ctx context.Context) {
	prune := func() {
		if err := s.store.PruneActivity(ctx, time.Now().UTC().AddDate(0, 0, -90)); err != nil && ctx.Err() == nil {
			log.Printf("prune old sandbox activity: %v", err)
		}
	}
	prune()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}

func (s *Server) sandboxActivity(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 100")
			return
		}
		limit = n
	}
	category := r.URL.Query().Get("category")
	if category != "" && category != "lifecycle" && category != "command" && category != "agent" && category != "access" && category != "snapshot" {
		writeError(w, http.StatusBadRequest, "invalid_category", "category must be lifecycle, command, agent, access, or snapshot")
		return
	}
	before, err := decodeActivityCursor(r.URL.Query().Get("before"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cursor", "before must be a valid activity cursor")
		return
	}
	var beforeTime *time.Time
	var beforeID string
	if before != nil {
		beforeTime = &before.time
		beforeID = before.id
	}
	items, err := s.store.ListActivityPage(r.Context(), pathID(r), category, beforeTime, beforeID, limit+1)
	if err != nil {
		s.apiError(w, err)
		return
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	responseItems := make([]activityItem, 0, len(items))
	for _, item := range items {
		detail := item.Detail
		if len(detail) == 0 {
			detail = json.RawMessage(`{}`)
		}
		responseItems = append(responseItems, activityItem{ID: item.ID, Time: item.Time, ActorType: item.ActorType,
			ActorLabel: item.ActorLabel, Category: item.Category, Action: item.Action, Detail: detail,
			DurationMS: item.DurationMS, Outcome: item.Outcome})
	}
	nextCursor := ""
	if hasMore && len(items) != 0 {
		last := items[len(items)-1]
		nextCursor = encodeActivityCursor(last.Time, last.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": responseItems, "nextCursor": nextCursor})
}

func encodeActivityCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "\x00" + id))
}

func decodeActivityCursor(value string) (*activityCursor, error) {
	if value == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil {
		stamp, id, ok := strings.Cut(string(decoded), "\x00")
		if ok && id != "" {
			at, parseErr := time.Parse(time.RFC3339Nano, stamp)
			if parseErr == nil {
				return &activityCursor{time: at.UTC(), id: id}, nil
			}
		}
	}
	// Accept an RFC3339 timestamp cursor from early clients. Opaque cursors
	// emitted by this API include an id so equal-time rows are not skipped.
	at, parseErr := time.Parse(time.RFC3339Nano, value)
	if parseErr == nil {
		return &activityCursor{time: at.UTC()}, nil
	}
	return nil, fmt.Errorf("invalid activity cursor")
}

func (s *Server) getPricing(w http.ResponseWriter, r *http.Request) {
	if currentUser(r).Role != "owner" {
		writeError(w, http.StatusForbidden, "forbidden", "owner role required")
		return
	}
	writeJSON(w, http.StatusOK, s.readPricing(r.Context()))
}

func (s *Server) putPricing(w http.ResponseWriter, r *http.Request) {
	if currentUser(r).Role != "owner" {
		writeError(w, http.StatusForbidden, "forbidden", "owner role required")
		return
	}
	var p pricingSettings
	if decodeJSON(r, &p) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid pricing settings")
		return
	}
	if p.Currency == "" {
		p.Currency = "USD"
	}
	if len(p.HourlyBySize) == 0 {
		p.HourlyBySize = map[string]float64{}
	}
	validSizes := map[string]bool{"xsmall": true, "small": true, "default": true, "large": true, "xlarge": true}
	for size, rate := range p.HourlyBySize {
		if !validSizes[size] || rate < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "hourly prices must use supported sizes and be nonnegative")
			return
		}
	}
	b, _ := json.Marshal(p)
	if err := s.store.SetSetting(r.Context(), "pricing", b); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) readPricing(ctx context.Context) pricingSettings {
	b, err := s.store.GetSetting(ctx, "pricing")
	if err != nil {
		return pricingSettings{Currency: "USD", HourlyBySize: map[string]float64{}}
	}
	var p pricingSettings
	if json.Unmarshal(b, &p) != nil {
		return pricingSettings{Currency: "USD", HourlyBySize: map[string]float64{}}
	}
	if p.Currency == "" {
		p.Currency = "USD"
	}
	if p.HourlyBySize == nil {
		p.HourlyBySize = map[string]float64{}
	}
	return p
}

var (
	sensitiveCommandArg = regexp.MustCompile(`(?i)(--?(?:password|token|secret|api[-_]?key|access[-_]?key|authorization))(?:\s+|=)([^\s]+)`)
	sensitiveKeyValue   = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:password|token|secret|api[_-]?key|access[_-]?key|authorization|credential)[a-z0-9_.-]*\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s&,;]+)`)
	environmentAssign   = regexp.MustCompile(`\b([A-Z_][A-Z0-9_]*=)("[^"]*"|'[^']*'|[^\s;&]+)`)
)

func safeActivityText(value string, maxRunes int) string {
	value = diagnostics.Sanitize(value)
	value = sensitiveKeyValue.ReplaceAllString(value, `${1}[REDACTED]`)
	value = environmentAssign.ReplaceAllString(value, `${1}[REDACTED]`)
	value = sensitiveCommandArg.ReplaceAllString(value, `${1}=[REDACTED]`)
	if maxRunes > 0 {
		runes := []rune(value)
		if len(runes) > maxRunes {
			value = string(runes[:maxRunes])
		}
	}
	return value
}

func safeActivityDetail(value any) json.RawMessage {
	if value == nil {
		return json.RawMessage(`{}`)
	}
	clean := sanitizeActivityValue(value)
	encoded, err := json.Marshal(clean)
	if err != nil || len(encoded) == 0 {
		return json.RawMessage(`{}`)
	}
	return encoded
}

func sanitizeActivityValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, item := range value {
			normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
			if strings.Contains(normalized, "env") || strings.Contains(normalized, "token") || strings.Contains(normalized, "password") || strings.Contains(normalized, "secret") || strings.Contains(normalized, "apikey") || strings.Contains(normalized, "accesskey") || strings.Contains(normalized, "credential") || normalized == "content" || normalized == "privatekey" || normalized == "prompt" {
				continue
			}
			result[key] = sanitizeActivityValue(item)
		}
		return result
	case []any:
		result := make([]any, len(value))
		for index, item := range value {
			result[index] = sanitizeActivityValue(item)
		}
		return result
	case string:
		return safeActivityText(value, 0)
	default:
		return value
	}
}

type activityActor struct{ Type, ID, Label string }

func actorFromContext(ctx context.Context) activityActor {
	if value := ctx.Value(activityActorContextKey); value != nil {
		if actor, ok := value.(activityActor); ok {
			return actor
		}
	}
	if value := ctx.Value(userContextKey); value != nil {
		if user, ok := value.(types.User); ok {
			return activityActor{Type: "user", ID: user.ID, Label: user.Email}
		}
	}
	return activityActor{Type: "system", Label: "system"}
}

func (s *Server) recordActivity(ctx context.Context, boxID, category, action string, detail any, outcome string) {
	s.recordActivityAs(ctx, boxID, actorFromContext(ctx), category, action, detail, nil, outcome, time.Now().UTC())
}

func (s *Server) recordActivityWithMetrics(ctx context.Context, boxID, category, action string, detail any, durationMS *int64, outcome string, at time.Time) {
	s.recordActivityAs(ctx, boxID, actorFromContext(ctx), category, action, detail, durationMS, outcome, at)
}

func (s *Server) recordSystemActivity(ctx context.Context, boxID, actorLabel, category, action string, detail any, outcome string) {
	if strings.TrimSpace(actorLabel) == "" {
		actorLabel = "system"
	}
	s.recordActivityAs(ctx, boxID, activityActor{Type: "system", Label: actorLabel}, category, action, detail, nil, outcome, time.Now().UTC())
}

func (s *Server) recordActivityAs(ctx context.Context, boxID string, actor activityActor, category, action string, detail any, durationMS *int64, outcome string, at time.Time) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var outcomeValue *string
	if outcome != "" {
		outcomeValue = &outcome
	}
	_ = s.store.RecordActivity(ctx, store.Activity{ID: types.NewID("act_"), BoxID: boxID, Time: at,
		ActorType: actor.Type, ActorID: actor.ID, ActorLabel: actor.Label, Category: category, Action: action,
		Detail: safeActivityDetail(detail), DurationMS: durationMS, Outcome: outcomeValue})
}
