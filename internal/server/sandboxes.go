package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/tony1908/paddock/internal/store"
	"github.com/tony1908/paddock/internal/types"
)

type createSandboxRequest struct {
	Type            string               `json:"type"`
	Image           string               `json:"image"`
	TTLSeconds      json.RawMessage      `json:"ttlSeconds"`
	IdleStopSeconds json.RawMessage      `json:"idleStopSeconds"`
	Env             map[string]string    `json:"env"`
	SetupScript     *string              `json:"setupScript"`
	Environment     string               `json:"environment"`
	NoEnv           bool                 `json:"noEnv"`
	NoSecrets       bool                 `json:"noSecrets"`
	FromSnapshot    string               `json:"fromSnapshot"`
	From            string               `json:"from"`
	Tags            map[string]string    `json:"tags"`
	NetworkPolicy   *types.NetworkPolicy `json:"networkPolicy"`
	Source          *sandboxSource       `json:"source"`
	KeepSnapshots   int                  `json:"keepSnapshots"`
	Mounts          []types.Mount        `json:"mounts"`
	Fork            bool                 `json:"-"`
}

const (
	defaultSourcePath       = "project"
	sourceTokenSecret       = "source-token"
	sourceTokenPath         = "/run/paddock/secrets/" + sourceTokenSecret
	maximumSourceTokenBytes = 4096
	maximumSourceDepth      = 1000
)

var sourceRevisionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)

// sandboxSource is the request form of a project source. Token is never persisted on the box.
type sandboxSource struct {
	Type     string `json:"type"`
	URL      string `json:"url"`
	Revision string `json:"revision,omitempty"`
	Depth    *int   `json:"depth,omitempty"`
	Token    string `json:"token,omitempty"`
	Path     string `json:"path,omitempty"`
}

// validateSandboxSource checks a source request and fills defaults. Only https URLs are accepted.
func validateSandboxSource(source sandboxSource) (sandboxSource, error) {
	if source.Path == "" {
		source.Path = defaultSourcePath
	}
	if source.Path[0] == '/' || strings.ContainsAny(source.Path, "\\\x00") {
		return sandboxSource{}, errors.New("source path must be a relative directory name")
	}
	for _, segment := range strings.Split(source.Path, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") {
			return sandboxSource{}, errors.New("source path must be a relative directory name without dot segments")
		}
	}
	parsed, err := url.Parse(source.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return sandboxSource{}, errors.New("source url must be an https URL without embedded credentials")
	}
	switch source.Type {
	case "git":
		if source.Revision != "" && !sourceRevisionPattern.MatchString(source.Revision) {
			return sandboxSource{}, errors.New("source revision must be a branch, tag, or commit SHA")
		}
		if source.Depth != nil && (*source.Depth < 1 || *source.Depth > maximumSourceDepth) {
			return sandboxSource{}, fmt.Errorf("source depth must be between 1 and %d", maximumSourceDepth)
		}
		if len(source.Token) > maximumSourceTokenBytes || strings.ContainsAny(source.Token, "\r\n\x00") {
			return sandboxSource{}, fmt.Errorf("source token must be at most %d bytes on one line", maximumSourceTokenBytes)
		}
	case "tarball":
		if source.Revision != "" || source.Depth != nil || source.Token != "" {
			return sandboxSource{}, errors.New("tarball sources accept only type, url, and path")
		}
	default:
		return sandboxSource{}, errors.New("source type must be git or tarball")
	}
	return source, nil
}

func (s sandboxSource) boxSource() *types.BoxSource {
	return &types.BoxSource{Type: s.Type, URL: s.URL, Revision: s.Revision, Path: s.Path}
}

func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request) {
	var input createSandboxRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid sandbox request")
		return
	}
	if err := types.ValidateTags(input.Tags); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_tags", err.Error())
		return
	}
	if input.NetworkPolicy != nil {
		if err := types.ValidateNetworkPolicy(*input.NetworkPolicy); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_network_policy", err.Error())
			return
		}
	}
	if input.Image != "" {
		if err := validateImageReference(input.Image); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_image", err.Error())
			return
		}
	}
	if !validKeepSnapshots(input.KeepSnapshots) {
		writeError(w, http.StatusBadRequest, "invalid_keep_snapshots", "keepSnapshots must be between 1 and 50, or 0 for the default")
		return
	}
	if err := s.checkMounts(r.Context(), input.Mounts, ""); err != nil {
		s.writeDriveError(w, err)
		return
	}
	if input.Source != nil {
		source, err := validateSandboxSource(*input.Source)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_source", err.Error())
			return
		}
		input.Source = &source
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key != "" {
		scope := currentUser(r).ID + ":sandbox.create"
		previous, err := s.store.GetIdempotency(r.Context(), key)
		if err == nil {
			if previous.Scope != scope {
				writeError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was used for another request")
				return
			}
			var response sandboxResponse
			if json.Unmarshal(previous.Response, &response) == nil {
				if response.TTLSeconds != nil {
					response.Box.TTLSeconds = *response.TTLSeconds
				}
				response.Box.ExpiresAt = response.ExpiresAt
				writeJSON(w, http.StatusCreated, response)
				return
			}
			writeError(w, http.StatusConflict, "idempotency_conflict", "stored idempotency response is invalid")
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			s.apiError(w, err)
			return
		}
		existing, boxErr := s.store.BoxByIdempotencyKey(r.Context(), key)
		if boxErr == nil {
			response, viewErr := s.sandboxView(r.Context(), existing)
			if viewErr != nil {
				s.apiError(w, viewErr)
				return
			}
			writeJSON(w, http.StatusCreated, response)
			return
		}
		if !errors.Is(boxErr, sql.ErrNoRows) {
			s.apiError(w, boxErr)
			return
		}
	}
	box, err := s.provisionSandbox(r.Context(), input, "", key)
	if err != nil {
		s.emitWebhookEvent(context.Background(), "sandbox.error", map[string]any{"error": err.Error()})
		status := http.StatusBadGateway
		code := "provision_failed"
		message := "sandbox could not be provisioned"
		var noCapacity *capacityError
		if errors.As(err, &noCapacity) {
			status, code, message = http.StatusConflict, "no_capacity", err.Error()
		} else if strings.Contains(err.Error(), "unknown box size") {
			status, code, message = http.StatusBadRequest, "invalid_type", err.Error()
		} else if strings.Contains(err.Error(), "ttlSeconds") {
			status, code, message = http.StatusBadRequest, "invalid_ttl", err.Error()
		} else if strings.Contains(err.Error(), "idleStopSeconds") {
			status, code, message = http.StatusBadRequest, "invalid_idle_stop", err.Error()
		} else if strings.Contains(err.Error(), "setupScript") {
			status, code, message = http.StatusBadRequest, "invalid_setup_script", err.Error()
		} else if strings.Contains(err.Error(), "env") {
			status, code, message = http.StatusBadRequest, "invalid_env", err.Error()
		} else if agentErrorCode(err) == "box_not_ready" {
			status, code, message = http.StatusServiceUnavailable, "box_not_ready", withAgentError("boxd did not become ready", err)
		} else if agentErrorCode(err) != "" {
			message = withAgentError(message, err)
		}
		writeError(w, status, code, message)
		return
	}
	if key != "" {
		record, getErr := s.store.GetBox(r.Context(), box.ID)
		if getErr != nil {
			s.apiError(w, getErr)
			return
		}
		response, viewErr := s.sandboxView(r.Context(), record)
		if viewErr != nil {
			s.apiError(w, viewErr)
			return
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			s.apiError(w, err)
			return
		}
		if err := s.store.PutIdempotency(r.Context(), store.IdempotencyRecord{Key: key, Scope: currentUser(r).ID + ":sandbox.create", Response: encoded, CreatedAt: time.Now().UTC()}); err != nil {
			s.apiError(w, err)
			return
		}
	}
	record, err := s.store.GetBox(r.Context(), box.ID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	response, err := s.sandboxView(r.Context(), record)
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) provisionSandbox(ctx context.Context, input createSandboxRequest, forcedSnapshot, idempotencyKey string) (types.Box, error) {
	boxType := input.Type
	if boxType == "" {
		boxType = "default"
	}
	size, err := types.SizeFor(boxType)
	if err != nil {
		return types.Box{}, err
	}
	ttl, err := parseTTLSeconds(input.TTLSeconds, defaultTTLSeconds)
	if err != nil {
		return types.Box{}, err
	}
	idleStop, err := parseIdleStopSeconds(input.IdleStopSeconds, 0)
	if err != nil {
		return types.Box{}, err
	}
	if err := validateSandboxEnv(input.Env); err != nil {
		return types.Box{}, err
	}
	if input.SetupScript != nil && len(*input.SetupScript) > maximumSetupBytes {
		return types.Box{}, fmt.Errorf("setupScript must not exceed %d bytes", maximumSetupBytes)
	}
	fromSnapshot := forcedSnapshot
	var restoreStorage map[string]any
	if fromSnapshot == "" {
		fromSnapshot = input.FromSnapshot
	}
	if fromSnapshot == "" && input.From != "" {
		named, lookupErr := s.store.GetNamedSnapshot(ctx, input.From)
		if lookupErr == nil {
			snapshot, getErr := s.store.GetSnapshot(ctx, named.SnapshotID)
			if getErr != nil {
				return types.Box{}, getErr
			}
			fromSnapshot = snapshot.ResticID
			restoreStorage, err = s.snapshotStorage(snapshot)
			if err != nil {
				return types.Box{}, err
			}
		} else if snapshot, getErr := s.store.GetSnapshot(ctx, input.From); getErr == nil {
			fromSnapshot = snapshot.ResticID
			restoreStorage, err = s.snapshotStorage(snapshot)
			if err != nil {
				return types.Box{}, err
			}
		} else {
			return types.Box{}, lookupErr
		}
	}
	node, err := s.schedule(ctx, size, boxType)
	if err != nil {
		return types.Box{}, err
	}
	client, connected := newAgentClient(s.hub, node.Node.ID)
	if !connected {
		return types.Box{}, errors.New("local agent disconnected")
	}
	boxID := types.NewID("bx_")
	desktopPassword, err := randomToken(24)
	if err != nil {
		return types.Box{}, err
	}
	boxdToken, err := randomToken(32)
	if err != nil {
		return types.Box{}, err
	}
	passwordEnc, err := s.secrets.encrypt([]byte(desktopPassword))
	if err != nil {
		return types.Box{}, err
	}
	boxdEnc, err := s.secrets.encrypt([]byte(boxdToken))
	if err != nil {
		return types.Box{}, err
	}
	now := time.Now().UTC()
	image := input.Image
	if image == "" {
		image = s.defaultImages().Box
	}
	box := types.Box{ID: boxID, NodeID: node.Node.ID, Type: boxType, State: types.StateProvisioning, Image: image,
		TTLSeconds: ttl, IdleStopSeconds: idleStop, CreatedAt: now, FromSnapshot: fromSnapshot, Tags: input.Tags, NetworkPolicy: input.NetworkPolicy, KeepSnapshots: input.KeepSnapshots, Mounts: input.Mounts}
	if ttl > 0 {
		expires := now.Add(time.Duration(ttl) * time.Second)
		box.ExpiresAt = &expires
	}
	if input.Environment != "" && !input.NoEnv {
		box.EnvironmentID = input.Environment
	}
	payload := map[string]any{
		"boxId": box.ID, "image": box.Image, "toolsImage": s.defaultImages().Tools, "type": box.Type,
		"desktopPassword": desktopPassword, "boxdToken": boxdToken,
	}
	if fromSnapshot != "" {
		payload["fromSnapshot"] = fromSnapshot
	}
	if box.NetworkPolicy != nil {
		payload["networkPolicy"] = box.NetworkPolicy
	}
	if len(box.Mounts) != 0 {
		payload["mounts"] = box.Mounts
	}
	if restoreStorage != nil {
		payload["storage"] = restoreStorage
	}
	if input.Fork {
		payload["fork"] = true
	}
	if box.EnvironmentID != "" {
		environment, err := s.store.GetEnvironment(ctx, box.EnvironmentID)
		if err != nil {
			return types.Box{}, err
		}
		files, version, err := s.environmentFiles(ctx, environment.ID, environment.Version, input.NoSecrets)
		if err != nil {
			return types.Box{}, err
		}
		payload["files"] = files
		box.EnvironmentVersion = version
	}
	if input.Source != nil {
		box.Source = input.Source.boxSource()
	}
	if len(input.Env) != 0 || input.SetupScript != nil || input.Source != nil {
		files, err := mergeProvisionFiles(payloadFiles(payload), input.Env, input.SetupScript, input.Source)
		if err != nil {
			return types.Box{}, err
		}
		payload["files"] = files
	}
	files, _ := payload["files"].([]map[string]any)
	setupStatus := "none"
	if setupConfigured(files) {
		setupStatus = "running"
	}
	var created struct {
		ContainerID string `json:"containerId"`
	}
	ctx, cancel := agentTimeoutContext(ctx)
	defer cancel()
	record := store.BoxRecord{Box: box, DesktopPasswordEnc: passwordEnc, BoxdTokenEnc: boxdEnc, IdempotencyKey: idempotencyKey, SetupStatus: setupStatus, NoSecrets: input.NoSecrets}
	if len(input.Env) != 0 {
		encodedEnv, err := json.Marshal(input.Env)
		if err != nil {
			return types.Box{}, err
		}
		if record.RuntimeEnvEnc, err = s.secrets.encrypt(encodedEnv); err != nil {
			return types.Box{}, err
		}
	}
	if err := s.store.CreateBox(ctx, record); err != nil {
		return types.Box{}, fmt.Errorf("persist provisioning box: %w", err)
	}
	if err := client.json(ctx, http.MethodPost, "/v1/boxes", payload, &created); err != nil {
		s.recordActivity(ctx, box.ID, "lifecycle", "error", map[string]any{"reason": err.Error()}, "error")
		_ = s.store.DeleteBox(context.Background(), box.ID)
		return types.Box{}, fmt.Errorf("create box on agent: %w", err)
	}
	box.State = types.StateReady
	startedAt := time.Now().UTC()
	record.Box = box
	record.RunningSince = &startedAt
	if err := s.store.UpdateBox(ctx, record); err != nil {
		if response, cleanupErr := client.raw(context.Background(), http.MethodDelete, agentPath("v1", "boxes", box.ID), nil, ""); cleanupErr == nil {
			_ = response.Body.Close()
		}
		_ = s.store.DeleteBox(context.Background(), box.ID)
		return types.Box{}, fmt.Errorf("persist box: %w", err)
	}
	_ = s.store.OpenRunningInterval(ctx, box.ID, startedAt)
	s.recordActivity(ctx, box.ID, "lifecycle", "created", map[string]any{"type": box.Type, "node": box.NodeID}, "ok")
	s.recordActivity(ctx, box.ID, "lifecycle", "started/ready", map[string]any{"node": box.NodeID}, "ok")
	s.syncEventPump(box)
	eventName := "sandbox.ready"
	if box.FromSnapshot != "" {
		eventName = "sandbox.restored"
	}
	s.emitWebhookEvent(context.Background(), eventName, map[string]any{"sandbox": box})
	return box, nil
}

func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("filter")
	for _, letter := range filter {
		if !strings.ContainsRune("rse", letter) {
			writeError(w, http.StatusBadRequest, "invalid_filter", "filter may contain only r, s, and e")
			return
		}
	}
	tagFilters, err := parseTagFilters(r.URL.Query()["tag"])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_tags", err.Error())
		return
	}
	boxes, err := s.store.ListBoxes(r.Context(), "")
	if err != nil {
		s.apiError(w, err)
		return
	}
	items := make([]sandboxResponse, 0, len(boxes))
	for _, box := range boxes {
		if !matchingSandboxFilter(box.State, filter) || !matchingTagFilters(box.Tags, tagFilters) {
			continue
		}
		view, err := s.sandboxView(r.Context(), box)
		if err != nil {
			s.apiError(w, err)
			return
		}
		items = append(items, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": items})
}

// tagFilter matches boxes whose tag Key exists and, when HasValue is set, equals Value.
type tagFilter struct {
	Key      string
	Value    string
	HasValue bool
}

// parseTagFilters parses repeated ?tag=key=value (or ?tag=key) query values.
func parseTagFilters(values []string) ([]tagFilter, error) {
	filters := make([]tagFilter, 0, len(values))
	for _, value := range values {
		key, val, hasValue := strings.Cut(value, "=")
		if key == "" {
			return nil, fmt.Errorf("tag filter %q must be key=value or key", value)
		}
		filters = append(filters, tagFilter{Key: key, Value: val, HasValue: hasValue})
	}
	return filters, nil
}

func matchingTagFilters(tags map[string]string, filters []tagFilter) bool {
	for _, filter := range filters {
		value, ok := tags[filter.Key]
		if !ok || (filter.HasValue && value != filter.Value) {
			return false
		}
	}
	return true
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request) {
	box, err := s.store.GetBox(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	view, err := s.sandboxView(r.Context(), box)
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandbox": view})
}

func (s *Server) sandboxUsage(w http.ResponseWriter, r *http.Request) {
	box, err := s.store.GetBox(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	now := time.Now().UTC()
	total, month, days, err := s.store.RunningUsage(r.Context(), box.ID, now)
	if err != nil {
		s.apiError(w, err)
		return
	}
	legacyTotal := box.RunningSeconds
	if box.RunningSince != nil {
		start := *box.RunningSince
		if start.After(now) {
			start = now
		}
		legacyTotal += int64(now.Sub(start).Seconds())
		if len(days) == 0 {
			monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
			monthSince := start
			if monthSince.Before(monthStart) {
				monthSince = monthStart
			}
			if now.After(monthSince) {
				month += int64(now.Sub(monthSince).Seconds())
			}
			window := now.AddDate(0, 0, -29)
			window = time.Date(window.Year(), window.Month(), window.Day(), 0, 0, 0, 0, time.UTC)
			daySince := *box.RunningSince
			if daySince.Before(window) {
				daySince = window
			}
			if daySince.After(now) {
				daySince = now
			}
			for day := time.Date(daySince.Year(), daySince.Month(), daySince.Day(), 0, 0, 0, 0, time.UTC); day.Before(now); day = day.AddDate(0, 0, 1) {
				lo, hi := daySince, now
				if day.After(lo) {
					lo = day
				}
				if next := day.AddDate(0, 0, 1); next.Before(hi) {
					hi = next
				}
				if hi.After(lo) {
					days[day.Format("2006-01-02")] += int64(hi.Sub(lo).Seconds())
				}
			}
		}
	}
	if legacyTotal > total {
		total = legacyTotal
	}
	perDay := make([]map[string]any, 0, 30)
	for i := 29; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("2006-01-02")
		perDay = append(perDay, map[string]any{"date": d, "runningSeconds": days[d]})
	}
	counts, err := s.store.ActivityCounts(r.Context(), box.ID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	snapshots, err := s.store.ListSnapshots(r.Context(), box.ID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	usage := map[string]any{"runningSeconds": total, "runningSecondsThisMonth": month, "perDay": perDay,
		"commandsCount": counts.Commands, "agentRunsCount": counts.AgentRuns, "desktopSessionsCount": counts.DesktopSessions,
		"snapshotsCount": len(snapshots), "snapshotsBytes": int64(0), "lastSnapshotAt": nil, "since": box.RunningSince}
	if len(snapshots) > 0 {
		usage["snapshotsBytes"] = snapshots[len(snapshots)-1].SizeBytes
		usage["lastSnapshotAt"] = snapshots[len(snapshots)-1].CreatedAt
	}
	// totals = accumulated at each stop + the running container's live counters (when the agent answers).
	totals := boxCounters{CPUSeconds: box.TotalCPUSeconds, RxBytes: box.TotalRxBytes, TxBytes: box.TotalTxBytes}
	if live, ok := s.liveCounters(r.Context(), box); ok {
		usage["live"] = live
		totals = totals.plus(live)
	}
	usage["totals"] = totals
	pricing := s.pricing(r.Context())
	rate, configured := pricing.HourlyBySize[box.Type]
	if !configured {
		rate = pricing.HourlyBySize["default"]
		configured = rate > 0
	}
	if configured {
		usage["estimatedCost"] = map[string]any{"currency": pricing.Currency, "amount": float64(total) * rate / 3600, "hourlyRate": rate}
	}
	writeJSON(w, http.StatusOK, usage)
}

func (s *Server) patchSandbox(w http.ResponseWriter, r *http.Request) {
	var input struct {
		TTLSeconds      json.RawMessage   `json:"ttlSeconds"`
		IdleStopSeconds json.RawMessage   `json:"idleStopSeconds"`
		Tags            map[string]string `json:"tags"`
		NetworkPolicy   json.RawMessage   `json:"networkPolicy"`
		KeepSnapshots   *int              `json:"keepSnapshots"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid sandbox update")
		return
	}
	if err := types.ValidateTags(input.Tags); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_tags", err.Error())
		return
	}
	if input.KeepSnapshots != nil && !validKeepSnapshots(*input.KeepSnapshots) {
		writeError(w, http.StatusBadRequest, "invalid_keep_snapshots", "keepSnapshots must be between 1 and 50, or 0 for the default")
		return
	}
	// An absent networkPolicy leaves the policy alone; null clears it.
	var policyUpdate *types.NetworkPolicy
	policyChanged := input.NetworkPolicy != nil
	if policyChanged && string(input.NetworkPolicy) != "null" {
		var policy types.NetworkPolicy
		if err := json.Unmarshal(input.NetworkPolicy, &policy); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_network_policy", "networkPolicy must be an object or null")
			return
		}
		if err := types.ValidateNetworkPolicy(policy); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_network_policy", err.Error())
			return
		}
		policyUpdate = &policy
	}
	box, err := s.store.GetBox(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	oldTTL := box.TTLSeconds
	oldIdleStop := box.IdleStopSeconds
	ttlChanged := false
	if input.TTLSeconds != nil {
		ttl, err := parseTTLSeconds(input.TTLSeconds, 0)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_ttl", err.Error())
			return
		}
		box.TTLSeconds = ttl
		box.ExpiresAt = nil
		if ttl > 0 {
			expires := time.Now().UTC().Add(time.Duration(ttl) * time.Second)
			box.ExpiresAt = &expires
		}
		ttlChanged = oldTTL != ttl
	}
	if input.IdleStopSeconds != nil {
		seconds, err := parseIdleStopSeconds(input.IdleStopSeconds, oldIdleStop)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_idle_stop", err.Error())
			return
		}
		box.IdleStopSeconds = seconds
	}
	if input.Tags != nil {
		// Replaces the whole map; an empty object clears all tags.
		box.Tags = input.Tags
	}
	if input.KeepSnapshots != nil {
		box.KeepSnapshots = *input.KeepSnapshots
	}
	if policyChanged {
		// A stopped box only stores the policy; it is sent on the next create (resume or start).
		if box.State == types.StateReady || box.State == types.StateIdle {
			client, connected := newAgentClient(s.hub, box.NodeID)
			if !connected {
				writeError(w, http.StatusBadGateway, "network_policy_failed", "box node is offline")
				return
			}
			var body any = json.RawMessage("null")
			if policyUpdate != nil {
				body = policyUpdate
			}
			if err := client.json(r.Context(), http.MethodPut, agentPath("v1", "boxes", box.ID, "network"), body, nil); err != nil {
				s.recordActivity(r.Context(), box.ID, "network", "error", map[string]any{"reason": err.Error()}, "error")
				writeError(w, http.StatusBadGateway, "network_policy_failed", withAgentError("network policy could not be applied", err))
				return
			}
		}
		box.NetworkPolicy = policyUpdate
		s.recordActivity(r.Context(), box.ID, "network", "policy changed", map[string]any{"mode": networkMode(policyUpdate)}, "ok")
	}
	if err := s.store.UpdateBox(r.Context(), box); err != nil {
		s.apiError(w, err)
		return
	}
	if ttlChanged {
		s.recordActivity(r.Context(), box.ID, "lifecycle", "TTL changed", map[string]any{"old": oldTTL, "new": box.TTLSeconds}, "ok")
	}
	if oldIdleStop != box.IdleStopSeconds {
		s.recordActivity(r.Context(), box.ID, "lifecycle", "idle stop changed", map[string]any{"old": oldIdleStop, "new": box.IdleStopSeconds}, "ok")
	}
	view, err := s.sandboxView(r.Context(), box)
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandbox": view})
}

func (s *Server) stopSandbox(w http.ResponseWriter, r *http.Request) {
	box, client, err := s.loadBoxAgent(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	if err := s.waitSnapshot(r.Context(), box.ID); err != nil {
		writeError(w, http.StatusConflict, "snapshot_in_progress", "sandbox snapshot capture did not finish before the wait expired")
		return
	}
	defer s.endSnapshot(box.ID)
	s.recordActivity(r.Context(), box.ID, "lifecycle", "stop requested", nil, "ok")
	// Read usage before the container stops; a failed read only leaves this session out of the totals.
	// ponytail: a crash between stops loses that session's usage numbers.
	sessionUsage, hasUsage := s.liveCounters(r.Context(), box)
	var response struct {
		Snapshot struct {
			SnapshotID string    `json:"snapshotId"`
			SizeBytes  int64     `json:"sizeBytes"`
			CreatedAt  time.Time `json:"createdAt"`
		} `json:"snapshot"`
	}
	if err := client.json(r.Context(), http.MethodPost, agentPath("v1", "boxes", box.ID, "stop"), map[string]bool{"snapshot": true}, &response); err != nil {
		s.recordActivity(r.Context(), box.ID, "lifecycle", "error", map[string]any{"reason": err.Error()}, "error")
		if agentErrorStatus(err) == http.StatusConflict {
			writeError(w, http.StatusConflict, "snapshot_failed", withAgentError("sandbox could not be captured and remains running", err))
			return
		}
		writeError(w, agentErrorStatus(err), "agent_error", withAgentError("agent could not stop sandbox", err))
		return
	}
	if response.Snapshot.SnapshotID == "" {
		s.recordActivity(r.Context(), box.ID, "lifecycle", "error", map[string]any{"reason": "agent returned no stop snapshot"}, "error")
		writeError(w, http.StatusBadGateway, "snapshot_failed", "agent returned no snapshot identifier; sandbox remains running")
		return
	}
	if hasUsage {
		box.TotalCPUSeconds += sessionUsage.CPUSeconds
		box.TotalRxBytes += sessionUsage.RxBytes
		box.TotalTxBytes += sessionUsage.TxBytes
	}
	now := time.Now().UTC()
	box.State = types.StateStopped
	s.dropDesktopSession(box.ID)
	box.StoppedAt = &now
	closeRunningInterval(&box, now)
	if response.Snapshot.SnapshotID != "" {
		createdAt := response.Snapshot.CreatedAt
		if createdAt.IsZero() {
			createdAt = now
		}
		if _, err := s.recordSnapshot(r.Context(), box.ID, response.Snapshot.SnapshotID, response.Snapshot.SizeBytes, createdAt, s.storageConfig()); err != nil {
			s.recordActivity(r.Context(), box.ID, "lifecycle", "error", map[string]any{"reason": err.Error()}, "error")
			s.apiError(w, err)
			return
		}
		s.recordActivity(r.Context(), box.ID, "snapshot", "stop capture", map[string]any{"sizeBytes": response.Snapshot.SizeBytes}, "ok")
	}
	if err := s.store.UpdateBox(r.Context(), box); err != nil {
		s.apiError(w, err)
		return
	}
	_ = s.store.CloseRunningInterval(r.Context(), box.ID, now)
	s.recordActivity(r.Context(), box.ID, "lifecycle", "stopped", nil, "ok")
	s.stopEventPump(box.ID)
	view, err := s.sandboxView(r.Context(), box)
	if err != nil {
		s.apiError(w, err)
		return
	}
	s.emitWebhookEvent(context.Background(), "sandbox.stopped", map[string]any{"sandbox": view})
	s.queueRetentionDelete(r.Context(), box)
	writeJSON(w, http.StatusOK, map[string]any{"sandbox": view})
}

type resumeSandboxRequest struct {
	Type       string          `json:"type"`
	TTLSeconds json.RawMessage `json:"ttlSeconds"`
}

var (
	errSnapshotRequired = errors.New("sandbox cannot resume without a snapshot")
	errSnapshotBusy     = errors.New("sandbox snapshot capture did not finish before the wait expired")
)

func (s *Server) resumeSandbox(w http.ResponseWriter, r *http.Request) {
	var input resumeSandboxRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid resume request")
			return
		}
	}
	box, err := s.store.GetBox(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	box, err = s.resumeBox(r.Context(), box, input)
	if err != nil {
		s.writeResumeError(w, box.ID, err)
		return
	}
	view, err := s.sandboxView(r.Context(), box)
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandbox": view})
}

func (s *Server) resumeBox(ctx context.Context, box store.BoxRecord, input resumeSandboxRequest) (store.BoxRecord, error) {
	oldTTL := box.TTLSeconds
	if err := s.waitSnapshot(ctx, box.ID); err != nil {
		return box, errSnapshotBusy
	}
	defer s.endSnapshot(box.ID)
	if input.Type != "" {
		if _, err := types.SizeFor(input.Type); err != nil {
			return box, err
		}
	}
	snapshots, err := s.store.ListSnapshots(ctx, box.ID)
	if err != nil {
		return box, err
	}
	if len(snapshots) == 0 {
		return box, errSnapshotRequired
	}
	ttl := box.TTLSeconds
	if input.TTLSeconds != nil {
		ttl, err = parseTTLSeconds(input.TTLSeconds, ttl)
		if err != nil {
			return box, err
		}
	}
	if input.Type == "" {
		input.Type = box.Type
	}
	box.State = types.StateResuming
	if err := s.store.UpdateBox(ctx, box); err != nil {
		return box, err
	}
	if err := s.migrateBox(ctx, &box, struct {
		Type string `json:"type"`
	}{Type: input.Type}); err != nil {
		s.recordActivity(ctx, box.ID, "lifecycle", "error", map[string]any{"reason": err.Error()}, "error")
		box.State = types.StateStopped
		_ = s.store.UpdateBox(context.Background(), box)
		s.emitWebhookEvent(context.Background(), "sandbox.error", map[string]any{"sandbox": box.Box, "error": err.Error()})
		return box, err
	}
	// The restore target is consumed by a successful resume.
	box.RestoreSnapshotID = ""
	now := time.Now().UTC()
	if box.State == types.StateProvisioning {
		box.StoppedAt = nil
		box.TTLSeconds = ttl
		box.Error = ""
		box.ErrorCode = ""
		box.ExpiresAt = nil
		if ttl > 0 {
			expires := now.Add(time.Duration(ttl) * time.Second)
			box.ExpiresAt = &expires
		}
		if err := s.store.UpdateBox(ctx, box); err != nil {
			return box, err
		}
		if oldTTL != ttl {
			s.recordActivity(ctx, box.ID, "lifecycle", "TTL changed", map[string]any{"old": oldTTL, "new": ttl}, "ok")
		}
		s.recordActivity(ctx, box.ID, "lifecycle", "resumed", map[string]any{"node": box.NodeID}, "ok")
		return box, nil
	}
	box.State = types.StateReady
	box.Error = ""
	box.ErrorCode = ""
	box.StoppedAt = nil
	box.TTLSeconds = ttl
	box.ExpiresAt = nil
	if ttl > 0 {
		expires := now.Add(time.Duration(ttl) * time.Second)
		box.ExpiresAt = &expires
	}
	box.RunningSince = &now
	if err := s.store.UpdateBox(ctx, box); err != nil {
		return box, err
	}
	_ = s.store.OpenRunningInterval(ctx, box.ID, now)
	if oldTTL != ttl {
		s.recordActivity(ctx, box.ID, "lifecycle", "TTL changed", map[string]any{"old": oldTTL, "new": ttl}, "ok")
	}
	s.recordActivity(ctx, box.ID, "lifecycle", "resumed", map[string]any{"node": box.NodeID}, "ok")
	s.syncEventPump(box.Box)
	return box, nil
}

// restoreSandbox rolls a box back to one of its own snapshots. A running box is stopped (capturing a
// final snapshot, so the rollback can be undone) and resumed from the chosen snapshot; a stopped box
// only records the snapshot, which the next resume uses.
func (s *Server) restoreSandbox(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SnapshotID string `json:"snapshotId"`
	}
	if err := decodeJSON(r, &input); err != nil || input.SnapshotID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "snapshotId is required")
		return
	}
	box, err := s.store.GetBox(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	switch box.State {
	case types.StateProvisioning, types.StateStopping, types.StateResuming, types.StateDeleting:
		writeError(w, http.StatusConflict, "sandbox_busy", "sandbox is "+string(box.State)+"; retry when it settles")
		return
	}
	snapshots, err := s.store.ListSnapshots(r.Context(), box.ID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	found := false
	for _, snapshot := range snapshots {
		found = found || snapshot.ID == input.SnapshotID
	}
	if !found {
		writeError(w, http.StatusNotFound, "snapshot_not_found", "snapshot not found for this sandbox")
		return
	}
	running := box.State == types.StateReady || box.State == types.StateIdle
	if running {
		// Reuse the stop handler; relay its response unchanged if the stop fails.
		recorder := httptest.NewRecorder()
		s.stopSandbox(recorder, r)
		if recorder.Code != http.StatusOK {
			for key, values := range recorder.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		if box, err = s.store.GetBox(r.Context(), box.ID); err != nil {
			s.apiError(w, err)
			return
		}
	}
	box.RestoreSnapshotID = input.SnapshotID
	if err := s.store.UpdateBox(r.Context(), box); err != nil {
		s.apiError(w, err)
		return
	}
	s.recordActivity(r.Context(), box.ID, "snapshot", "restore requested", map[string]any{"snapshotId": input.SnapshotID}, "ok")
	if running {
		if box, err = s.resumeBox(r.Context(), box, resumeSandboxRequest{}); err != nil {
			s.writeResumeError(w, box.ID, err)
			return
		}
	}
	view, err := s.sandboxView(r.Context(), box)
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandbox": view})
}

func (s *Server) writeResumeError(w http.ResponseWriter, boxID string, err error) {
	switch {
	case errors.Is(err, errSnapshotBusy):
		writeError(w, http.StatusConflict, "snapshot_in_progress", err.Error())
	case errors.Is(err, errSnapshotRequired):
		writeError(w, http.StatusConflict, "snapshot_required", err.Error())
	case strings.Contains(err.Error(), "ttlSeconds"):
		writeError(w, http.StatusBadRequest, "invalid_ttl", err.Error())
	case strings.Contains(err.Error(), "idleStopSeconds"):
		writeError(w, http.StatusBadRequest, "invalid_idle_stop", err.Error())
	case strings.Contains(err.Error(), "unknown box size"):
		writeError(w, http.StatusBadRequest, "invalid_type", err.Error())
	case agentErrorCode(err) == "box_not_ready":
		writeError(w, http.StatusServiceUnavailable, "box_not_ready", withAgentError("boxd did not become ready", err))
	default:
		var noCapacity *capacityError
		if errors.As(err, &noCapacity) {
			writeError(w, http.StatusConflict, "no_capacity", err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, "resume_failed", withAgentError("sandbox could not be resumed on the local machine", err))
	}
}

// runtimeFiles rebuilds the /run/paddock files a box needs when it starts again. Snapshots exclude /run,
// so resume and rollback must deliver them again. setup.json is dropped: setup never re-runs on resume.
func (s *Server) runtimeFiles(ctx context.Context, record store.BoxRecord) ([]map[string]any, error) {
	var files []map[string]any
	if record.EnvironmentID != "" {
		envFiles, _, err := s.environmentFiles(ctx, record.EnvironmentID, record.EnvironmentVersion, record.NoSecrets)
		if err != nil {
			return nil, err
		}
		files = envFiles
	}
	if len(record.RuntimeEnvEnc) != 0 {
		plain, err := s.secrets.decrypt(record.RuntimeEnvEnc)
		if err != nil {
			return nil, err
		}
		var env map[string]string
		if err := json.Unmarshal(plain, &env); err != nil {
			return nil, err
		}
		if files, err = mergeProvisionFiles(files, env, nil, nil); err != nil {
			return nil, err
		}
	}
	kept := make([]map[string]any, 0, len(files))
	for _, file := range files {
		if name, _ := file["path"].(string); name != "/run/paddock/setup.json" {
			kept = append(kept, file)
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	return kept, nil
}

func (s *Server) migrateBox(ctx context.Context, record *store.BoxRecord, input struct {
	Type string `json:"type"`
}) error {
	snapshots, err := s.store.ListSnapshots(ctx, record.ID)
	if err != nil || len(snapshots) == 0 {
		return errors.New("cannot migrate without a snapshot")
	}
	targetType := record.Type
	if input.Type != "" {
		targetType = input.Type
	}
	size, err := types.SizeFor(targetType)
	if err != nil {
		return err
	}
	node, err := s.scheduleExcluding(ctx, size, targetType, record.ID)
	if err != nil {
		return err
	}
	oldAgent, sourceConnected := newAgentClient(s.hub, record.NodeID)
	if !sourceConnected {
		return errors.New("local agent disconnected")
	}
	newAgent := oldAgent
	password, err := s.secrets.decrypt(record.DesktopPasswordEnc)
	if err != nil {
		return err
	}
	boxdToken, err := s.secrets.decrypt(record.BoxdTokenEnc)
	if err != nil {
		return err
	}
	source := snapshots[len(snapshots)-1]
	if record.RestoreSnapshotID != "" {
		found := false
		for _, snapshot := range snapshots {
			if snapshot.ID == record.RestoreSnapshotID {
				source, found = snapshot, true
			}
		}
		if !found {
			return errors.New("restore snapshot no longer exists")
		}
	}
	restoreStorage, err := s.snapshotStorage(source)
	if err != nil {
		return err
	}
	payload := map[string]any{"boxId": record.ID, "image": record.Image, "toolsImage": s.defaultImages().Tools, "type": targetType,
		"desktopPassword": string(password), "boxdToken": string(boxdToken), "fromSnapshot": source.ResticID,
		"storage": restoreStorage}
	if record.NetworkPolicy != nil {
		payload["networkPolicy"] = record.NetworkPolicy
	}
	if len(record.Mounts) != 0 {
		payload["mounts"] = record.Mounts
	}
	files, err := s.runtimeFiles(ctx, *record)
	if err != nil {
		return err
	}
	if len(files) != 0 {
		payload["files"] = files
	}
	if node.Node.ID == record.NodeID {
		if err := oldAgent.json(ctx, http.MethodDelete, agentPath("v1", "boxes", record.ID), nil, nil); err != nil {
			return err
		}
		if err := newAgent.json(ctx, http.MethodPost, "/v1/boxes", payload, nil); err != nil {
			return err
		}
	}
	record.NodeID = node.Node.ID
	record.Type = targetType
	record.State = types.StateReady
	_ = s.pushCurrentBoxRoutes(ctx, newAgent, record.Box)
	return nil
}

func (s *Server) forkSandbox(w http.ResponseWriter, r *http.Request) {
	var input createSandboxRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid fork request")
			return
		}
	}
	if input.Source != nil {
		writeError(w, http.StatusBadRequest, "invalid_source", "source is not supported for forks; the fork already has the files")
		return
	}
	if err := types.ValidateTags(input.Tags); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_tags", err.Error())
		return
	}
	if input.NetworkPolicy != nil {
		if err := types.ValidateNetworkPolicy(*input.NetworkPolicy); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_network_policy", err.Error())
			return
		}
	}
	source, client, err := s.loadBoxAgent(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	if err := s.waitSnapshot(r.Context(), source.ID); err != nil {
		s.recordActivity(r.Context(), source.ID, "lifecycle", "error", map[string]any{"reason": err.Error()}, "error")
		writeError(w, http.StatusConflict, "snapshot_in_progress", "source sandbox snapshot capture did not finish before the wait expired")
		return
	}
	defer s.endSnapshot(source.ID)
	var captured struct {
		SnapshotID string    `json:"snapshotId"`
		SizeBytes  int64     `json:"sizeBytes"`
		CreatedAt  time.Time `json:"createdAt"`
	}
	if err := client.json(r.Context(), http.MethodPost, agentPath("v1", "boxes", source.ID, "snapshot"), nil, &captured); err != nil {
		s.recordActivity(r.Context(), source.ID, "lifecycle", "error", map[string]any{"reason": err.Error()}, "error")
		writeError(w, agentErrorStatus(err), "snapshot_failed", "source sandbox could not be captured")
		return
	}
	if captured.SnapshotID == "" {
		s.recordActivity(r.Context(), source.ID, "lifecycle", "error", map[string]any{"reason": "agent returned no snapshot identifier"}, "error")
		writeError(w, http.StatusBadGateway, "snapshot_failed", "agent returned no snapshot identifier")
		return
	}
	now := captured.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if _, err := s.recordSnapshot(r.Context(), source.ID, captured.SnapshotID, captured.SizeBytes, now, s.storageConfig()); err != nil {
		s.recordActivity(r.Context(), source.ID, "lifecycle", "error", map[string]any{"reason": err.Error()}, "error")
		s.apiError(w, err)
		return
	}
	s.recordActivity(r.Context(), source.ID, "snapshot", "manual capture", map[string]any{"sizeBytes": captured.SizeBytes}, "ok")
	if input.Type == "" {
		input.Type = source.Type
	}
	input.Image = source.Image // a fork always runs the source's image
	if len(input.IdleStopSeconds) == 0 {
		input.IdleStopSeconds, _ = json.Marshal(source.IdleStopSeconds)
	}
	if input.Tags == nil {
		input.Tags = source.Tags
	}
	if input.NetworkPolicy == nil {
		input.NetworkPolicy = source.NetworkPolicy
	}
	if input.KeepSnapshots == 0 {
		input.KeepSnapshots = source.KeepSnapshots
	}
	// Forks keep read-only drive mounts only; read-write drives are not copied (see docs).
	input.Mounts = readOnlyMounts(source.Mounts)
	input.Fork = true
	fork, err := s.provisionSandbox(r.Context(), input, captured.SnapshotID, "")
	if err != nil {
		s.recordActivity(r.Context(), source.ID, "lifecycle", "error", map[string]any{"reason": err.Error()}, "error")
		s.emitWebhookEvent(context.Background(), "sandbox.error", map[string]any{"sandbox": source.Box, "error": err.Error()})
		if agentErrorCode(err) == "box_not_ready" {
			writeError(w, http.StatusServiceUnavailable, "box_not_ready", withAgentError("boxd did not become ready", err))
			return
		}
		var noCapacity *capacityError
		if errors.As(err, &noCapacity) {
			writeError(w, http.StatusConflict, "no_capacity", err.Error())
			return
		}
		if strings.Contains(err.Error(), "ttlSeconds") {
			writeError(w, http.StatusBadRequest, "invalid_ttl", err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, "fork_failed", withAgentError("sandbox could not be forked", err))
		return
	}
	s.recordActivity(r.Context(), source.ID, "lifecycle", "forked", map[string]any{"newBoxId": fork.ID, "sourceBoxId": source.ID}, "ok")
	record, err := s.store.GetBox(r.Context(), fork.ID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	view, err := s.sandboxView(r.Context(), record)
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"sandbox": view})
}

func (s *Server) deleteSandboxNow(w http.ResponseWriter, r *http.Request) {
	queued, queuedErr := s.store.GetBox(r.Context(), pathID(r))
	if queuedErr == nil {
		localDelete := false
		if queued.State == types.StateStopped {
			if node, err := s.store.GetNode(r.Context(), queued.NodeID); err == nil {
				localDelete = node.Node.Status == "offline"
			}
		}
		if localDelete {
			if err := s.deleteDetachedSandbox(r.Context(), queued); err != nil {
				s.apiError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	box, client, err := s.loadBoxAgent(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	box.State = types.StateDeleting
	if err := s.store.UpdateBox(r.Context(), box); err != nil {
		s.apiError(w, err)
		return
	}
	s.stopEventPump(box.ID)
	if err := client.json(r.Context(), http.MethodDelete, agentPath("v1", "boxes", box.ID), nil, nil); err != nil {
		box.State = types.StateReady
		_ = s.store.UpdateBox(context.Background(), box)
		writeError(w, agentErrorStatus(err), "agent_error", "agent could not delete sandbox")
		return
	}
	snapshots, _ := s.store.ListSnapshots(r.Context(), box.ID)
	s.recordActivity(r.Context(), box.ID, "lifecycle", "deleted", map[string]any{"boxId": box.ID}, "ok")
	if err := s.store.DeleteBox(r.Context(), box.ID); err != nil {
		s.apiError(w, err)
		return
	}
	s.dropDesktopSession(box.ID)
	s.emitWebhookEvent(context.Background(), "sandbox.deleted", map[string]any{"sandbox": box.Box})
	w.WriteHeader(http.StatusNoContent)
	go s.forgetSnapshotsAsync(snapshots, client)
}

func (s *Server) forgetSnapshotsAsync(snapshots []types.Snapshot, client *agentClient) {
	for _, snapshot := range snapshots {
		for attempt := 0; ; attempt++ {
			s.pruneMu.Lock()
			headers, err := s.snapshotStorageHeader(snapshot)
			if err == nil {
				err = client.jsonWithHeaders(context.Background(), http.MethodDelete, agentPath("v1", "snapshots", snapshot.ResticID), nil, nil, headers)
			}
			s.pruneMu.Unlock()
			if err == nil {
				break
			}
			delay := time.Second << min(attempt, 8)
			time.Sleep(delay)
		}
		_ = s.store.DeleteSnapshot(context.Background(), snapshot.ID)
	}
}

func (s *Server) deleteDetachedSandbox(ctx context.Context, record store.BoxRecord) error {
	snapshots, err := s.store.ListSnapshots(ctx, record.ID)
	if err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		headers, err := s.snapshotStorageHeader(snapshot)
		if err != nil {
			continue
		}
		for _, nodeID := range s.hub.Connected() {
			client, ok := newAgentClient(s.hub, nodeID)
			if !ok {
				continue
			}
			if err := client.jsonWithHeaders(ctx, http.MethodDelete, agentPath("v1", "snapshots", snapshot.ResticID), nil, nil, headers); err == nil {
				break
			}
		}
	}
	s.stopEventPump(record.ID)
	s.recordActivity(ctx, record.ID, "lifecycle", "deleted", map[string]any{"boxId": record.ID}, "ok")
	if err := s.store.DeleteBox(ctx, record.ID); err != nil {
		return err
	}
	s.emitWebhookEvent(context.Background(), "sandbox.deleted", map[string]any{"sandbox": record.Box})
	return nil
}

func (s *Server) loadBoxAgent(ctx context.Context, id string) (store.BoxRecord, *agentClient, error) {
	box, err := s.store.GetBox(ctx, id)
	if err != nil {
		return store.BoxRecord{}, nil, err
	}
	client, ok := newAgentClient(s.hub, box.NodeID)
	if !ok {
		return store.BoxRecord{}, nil, errors.New("box node is offline")
	}
	return box, client, nil
}

// networkMode names a policy's mode for activity records; nil means no policy.
func networkMode(policy *types.NetworkPolicy) string {
	if policy == nil {
		return "none"
	}
	if policy.Mode == "" {
		return types.NetworkAllowAll
	}
	return policy.Mode
}
