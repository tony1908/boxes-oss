package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tony1908/boxes-oss/internal/store"
	"github.com/tony1908/boxes-oss/internal/types"
)

type desktopSession struct {
	Token     string
	ExpiresAt time.Time
	Public    bool
	Host      string
	Browser   bool
	Profile   string
}

func (s *Server) dropDesktopSession(boxID string) {
	_ = s.store.DeleteDesktopSession(context.Background(), boxID)
	s.platformMu.Lock()
	delete(s.desktopSessions, boxID)
	delete(s.desktopRoutes, boxID)
	s.platformMu.Unlock()
}

func (s *Server) loadDesktopSessions(ctx context.Context) error {
	records, err := s.store.ListDesktopSessions(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		if !record.ExpiresAt.After(s.now()) {
			_ = s.store.DeleteDesktopSession(ctx, record.BoxID)
			continue
		}
		token, err := s.secrets.decrypt(record.TokenEnc)
		if err != nil {
			return err
		}
		session := desktopSession{Token: string(token), ExpiresAt: record.ExpiresAt, Public: record.Public, Host: record.Host, Browser: record.Browser, Profile: record.Profile}
		s.desktopSessions[record.BoxID] = session
		s.desktopRoutes[record.BoxID] = desktopRouteState{Host: record.Host, TokenHash: tokenHash(session.Token), ExpiresAt: record.ExpiresAt}
	}
	return nil
}

type desktopRouteState struct {
	Host      string
	TokenHash string
	ExpiresAt time.Time
}

func (s *Server) createDesktop(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Browser bool   `json:"browser"`
		Profile string `json:"profile"`
		Public  bool   `json:"public"`
		Renewal bool   `json:"renewal"`
	}
	if err := decodeJSON(r, &input); err != nil && r.ContentLength != 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid desktop request")
		return
	}
	box, client, err := s.loadBoxAgent(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	node, err := s.store.GetNode(r.Context(), box.NodeID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	hostBase := nodeHostBase(node.Node)
	if hostBase == "" {
		writeError(w, http.StatusConflict, "node_host_missing", "node requires host or hostBase to create a desktop URL")
		return
	}
	host := "desk-" + dnsBoxID(box.ID) + "." + hostBase
	token, err := randomToken(32)
	if err != nil {
		s.apiError(w, err)
		return
	}
	now := s.now().UTC()
	s.platformMu.Lock()
	session, exists := s.desktopSessions[box.ID]
	created := !exists || session.Public != input.Public
	reused := exists && session.Public == input.Public && session.ExpiresAt.After(now.Add(30*time.Minute))
	if exists && session.Public == input.Public && session.ExpiresAt.After(now.Add(30*time.Minute)) {
		token = session.Token
		host = session.Host
	}
	if !exists || session.Public != input.Public || !session.ExpiresAt.After(now.Add(30*time.Minute)) {
		ttl := s.cfg.DesktopSessionTTL
		if input.Public {
			ttl = 7 * 24 * time.Hour
		}
		session = desktopSession{Token: token, ExpiresAt: now.Add(ttl), Public: input.Public, Host: host, Browser: input.Browser, Profile: input.Profile}
		enc, err := s.secrets.encrypt([]byte(token))
		if err != nil {
			s.platformMu.Unlock()
			s.apiError(w, err)
			return
		}
		if err := s.store.SaveDesktopSession(r.Context(), store.DesktopSessionRecord{BoxID: box.ID, TokenEnc: enc, ExpiresAt: session.ExpiresAt, Public: session.Public, Host: session.Host, Browser: session.Browser, Profile: session.Profile}); err != nil {
			s.platformMu.Unlock()
			s.apiError(w, err)
			return
		}
		s.desktopSessions[box.ID] = session
	}
	s.platformMu.Unlock()
	expiresAt := session.ExpiresAt
	route := desktopRouteState{Host: host, TokenHash: tokenHash(token), ExpiresAt: expiresAt}
	s.platformMu.Lock()
	s.desktopRoutes[box.ID] = route
	s.platformMu.Unlock()
	ready, err := s.pushBoxRoutesReady(r.Context(), client, box.Box, &route)
	if err != nil {
		s.platformMu.Lock()
		delete(s.desktopRoutes, box.ID)
		delete(s.desktopSessions, box.ID)
		s.platformMu.Unlock()
		_ = s.store.DeleteDesktopSession(r.Context(), box.ID)
		s.apiError(w, err)
		return
	}
	if created && !input.Renewal {
		s.recordActivity(r.Context(), box.ID, "access", "desktop session created", map[string]any{"public": session.Public, "browser": session.Browser}, "ok")
	} else if reused && !input.Renewal {
		s.recordActivity(r.Context(), box.ID, "access", "desktop session reused", map[string]any{"public": session.Public, "browser": session.Browser}, "ok")
	}
	parsed := url.URL{Scheme: "https", Host: host, Path: "/"}
	query := url.Values{}
	query.Set("token", token)
	if session.Browser {
		query.Set("app", "browser")
		if session.Profile != "" {
			query.Set("profile", session.Profile)
		}
	}
	parsed.RawQuery = query.Encode()
	writeJSON(w, http.StatusOK, map[string]any{"url": parsed.String(), "expiresAt": expiresAt, "ready": ready})
}

func (s *Server) getDesktop(w http.ResponseWriter, r *http.Request) {
	box, err := s.store.GetBox(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	s.platformMu.Lock()
	session, ok := s.desktopSessions[box.ID]
	if ok && !session.ExpiresAt.After(s.now()) {
		delete(s.desktopSessions, box.ID)
		delete(s.desktopRoutes, box.ID)
		ok = false
	}
	s.platformMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "desktop_not_found", "desktop session not found")
		return
	}
	s.recordActivity(r.Context(), box.ID, "access", "desktop session reused", map[string]any{"public": session.Public, "browser": session.Browser}, "ok")
	parsed := url.URL{Scheme: "https", Host: session.Host, Path: "/"}
	query := url.Values{}
	query.Set("token", session.Token)
	if session.Browser {
		query.Set("app", "browser")
		if session.Profile != "" {
			query.Set("profile", session.Profile)
		}
	}
	parsed.RawQuery = query.Encode()
	writeJSON(w, http.StatusOK, map[string]any{"url": parsed.String(), "expiresAt": session.ExpiresAt})
}

func nodeHostBase(node types.Node) string {
	base := strings.TrimSpace(node.HostBase)
	if base != "" {
		base = strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
		return strings.TrimRight(strings.SplitN(base, "/", 2)[0], ".")
	}
	host := strings.TrimSpace(node.Host)
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return ""
	}
	return strings.TrimRight(host, ".") + ".sslip.io"
}

func hostURLFor(boxID string, port int, hostBase string) string {
	return "https://" + dnsBoxID(boxID) + "-" + strconv.Itoa(port) + "." + hostBase
}

func dnsBoxID(boxID string) string {
	return strings.ReplaceAll(boxID, "_", "-")
}

func (s *Server) pushBoxRoutes(ctx context.Context, client *agentClient, box types.Box) {
	_ = s.reconcileCurrentBoxRoutes(ctx, client, box)
}

func (s *Server) pushCurrentBoxRoutes(ctx context.Context, client *agentClient, box types.Box) error {
	return s.syncCurrentBoxRoutes(ctx, client, box, false)
}

func (s *Server) reconcileCurrentBoxRoutes(ctx context.Context, client *agentClient, box types.Box) error {
	return s.syncCurrentBoxRoutes(ctx, client, box, true)
}

func (s *Server) syncCurrentBoxRoutes(ctx context.Context, client *agentClient, box types.Box, clearIfEmpty bool) error {
	s.platformMu.Lock()
	route, ok := s.desktopRoutes[box.ID]
	if ok && !route.ExpiresAt.After(time.Now()) {
		delete(s.desktopRoutes, box.ID)
		ok = false
	}
	s.platformMu.Unlock()
	if !ok {
		route = desktopRouteState{}
	}
	hosts, err := s.store.ListHosts(ctx, box.ID)
	if err != nil {
		return err
	}
	if !ok && len(hosts) == 0 && !clearIfEmpty {
		return nil
	}
	return s.pushBoxRoutesWithClient(ctx, client, box, routePointer(route, ok))
}

func routePointer(route desktopRouteState, ok bool) *desktopRouteState {
	if !ok {
		return nil
	}
	return &route
}

func (s *Server) pushBoxRoutesWithClient(ctx context.Context, client *agentClient, box types.Box, desktop *desktopRouteState) error {
	_, err := s.pushBoxRoutesReady(ctx, client, box, desktop)
	return err
}

func (s *Server) pushBoxRoutesReady(ctx context.Context, client *agentClient, box types.Box, desktop *desktopRouteState) (bool, error) {
	node, err := s.store.GetNode(ctx, box.NodeID)
	if err != nil {
		return false, err
	}
	base := nodeHostBase(node.Node)
	if base == "" && (desktop != nil) {
		return false, errors.New("node public host is required to configure routes")
	}
	hosts, err := s.store.ListHosts(ctx, box.ID)
	if err != nil {
		return false, err
	}
	if base == "" && len(hosts) != 0 {
		return false, errors.New("node public host is required to configure routes")
	}
	request := map[string]any{"hosts": make([]map[string]any, 0, len(hosts))}
	if desktop != nil {
		request["desktop"] = map[string]any{"host": desktop.Host, "tokenHash": desktop.TokenHash, "expiresAt": desktop.ExpiresAt}
	}
	for _, host := range hosts {
		request["hosts"] = append(request["hosts"].([]map[string]any), map[string]any{
			"host": hostURLHost(box.ID, host.Port, base), "port": host.Port,
			"private": host.Private, "tokenHash": host.TokenHash,
		})
	}
	response, err := client.rawJSON(ctx, http.MethodPut, agentPath("v1", "boxes", box.ID, "routes"), request)
	if err != nil {
		return false, err
	}
	return !strings.EqualFold(response.Header.Get("X-Routes-Ready"), "false"), nil
}

func hostURLHost(boxID string, port int, hostBase string) string {
	return strings.TrimPrefix(hostURLFor(boxID, port, hostBase), "https://")
}

func (s *Server) repushNodeRoutes(ctx context.Context, nodeID string, client *agentClient) {
	boxes, err := s.store.ListBoxes(ctx, nodeID)
	if err != nil {
		return
	}
	for _, record := range boxes {
		if record.State != types.StateReady {
			continue
		}
		s.pushBoxRoutes(ctx, client, record.Box)
	}
}
