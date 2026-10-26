package agent

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type routesRequest struct {
	Desktop *desktopRoute `json:"desktop"`
	Hosts   []hostRoute   `json:"hosts"`
}

type desktopRoute struct {
	Host      string    `json:"host"`
	TokenHash string    `json:"tokenHash"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type hostRoute struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Private   bool   `json:"private"`
	TokenHash string `json:"tokenHash"`
}

var validRouteHost = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*(?::[0-9]{1,5})?$`)

func (a *Agent) putRoutes(w http.ResponseWriter, r *http.Request) {
	boxID := r.PathValue("id")
	var request routesRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid routes request")
		return
	}
	if err := validateRoutes(request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_routes", err.Error())
		return
	}
	if request.Desktop == nil && len(request.Hosts) == 0 {
		for _, extension := range []string{".yml", ".json"} {
			if err := os.Remove(filepath.Join(a.config.TraefikDir, "box-"+boxID+extension)); err != nil && !errors.Is(err, os.ErrNotExist) {
				writeError(w, http.StatusInternalServerError, "routes_write_failed", "cannot remove Traefik routes")
				return
			}
		}
		a.mu.Lock()
		for host, credential := range a.forwardAuth {
			if credential.boxID == boxID {
				delete(a.forwardAuth, host)
			}
		}
		a.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	content, credentials, err := a.renderRoutes(r.Context(), boxID, request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_routes", err.Error())
		return
	}
	if err := a.ensureTraefikDir(); err != nil {
		writeError(w, http.StatusInternalServerError, "routes_write_failed", "cannot create Traefik dynamic directory")
		return
	}
	path := filepath.Join(a.config.TraefikDir, "box-"+boxID+".yml")
	if err := atomicWrite(path, content, 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, "routes_write_failed", "cannot write Traefik routes")
		return
	}
	if err := os.Remove(filepath.Join(a.config.TraefikDir, "box-"+boxID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "routes_write_failed", "cannot remove legacy Traefik routes")
		return
	}
	a.mu.Lock()
	for host, credential := range a.forwardAuth {
		if credential.boxID == boxID {
			delete(a.forwardAuth, host)
		}
	}
	for host, credential := range credentials {
		a.forwardAuth[strings.ToLower(host)] = credential
	}
	a.mu.Unlock()
	if a.config.TraefikCheckAddr != "none" && !waitRoutesReady(r.Context(), a.config.TraefikCheckAddr, routeHosts(request), 10*time.Second, 250*time.Millisecond) {
		w.Header().Set("X-Routes-Ready", "false")
		log.Printf("traefik routes not ready box=%s", boxID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func routeHosts(request routesRequest) []string {
	var hosts []string
	if request.Desktop != nil {
		hosts = append(hosts, request.Desktop.Host)
	}
	for _, route := range request.Hosts {
		hosts = append(hosts, route.Host)
	}
	return hosts
}

func waitRoutesReady(ctx context.Context, address string, hosts []string, timeout, interval time.Duration) bool {
	if len(hosts) == 0 {
		return true
	}
	deadline := time.Now().Add(timeout)
	for {
		allReady := true
		for _, host := range hosts {
			if !routeReady(ctx, address, host) {
				allReady = false
				break
			}
		}
		if allReady {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

func routeReady(ctx context.Context, address, host string) bool {
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", address)
	}}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", nil)
	if err != nil {
		return false
	}
	req.Host = host
	response, err := client.Do(req)
	if err != nil {
		transport.CloseIdleConnections()
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
	_ = response.Body.Close()
	transport.CloseIdleConnections()
	return response.StatusCode != http.StatusNotFound || strings.TrimSpace(string(body)) != "404 page not found"
}

func validateRoutes(request routesRequest) error {
	seen := make(map[string]struct{})
	if request.Desktop != nil {
		if !validRouteHost.MatchString(request.Desktop.Host) || request.Desktop.TokenHash == "" {
			return errors.New("desktop route requires a valid host and tokenHash")
		}
		seen[strings.ToLower(request.Desktop.Host)] = struct{}{}
	}
	for _, host := range request.Hosts {
		if !validRouteHost.MatchString(host.Host) || host.Port < 1 || host.Port > 65535 {
			return errors.New("host routes require a valid host and port")
		}
		key := strings.ToLower(host.Host)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate route host %q", host.Host)
		}
		seen[key] = struct{}{}
		if host.Private && host.TokenHash == "" {
			return fmt.Errorf("private host %q requires tokenHash", host.Host)
		}
	}
	return nil
}

func (a *Agent) renderRoutes(ctx context.Context, boxID string, request routesRequest) ([]byte, map[string]authCredential, error) {
	if request.Desktop == nil && len(request.Hosts) == 0 {
		return nil, map[string]authCredential{}, nil
	}
	routers := make(map[string]any)
	middlewares := make(map[string]any)
	services := make(map[string]any)
	credentials := make(map[string]authCredential)
	addHTTPSRoute := func(name, host, service string, useAuth bool) {
		middle := []string{}
		if useAuth {
			middle = append(middle, "box-"+boxID+"-forward-auth")
		}
		routers[name] = map[string]any{
			"entryPoints": []string{"websecure"}, "rule": "Host(`" + host + "`)",
			"service": service, "middlewares": middle,
			"tls": map[string]string{"certResolver": "letsencrypt"},
		}
		routers[name+"-web"] = map[string]any{
			"entryPoints": []string{"web"}, "rule": "Host(`" + host + "`)",
			"service": service, "middlewares": []string{"box-" + boxID + "-redirect-https"},
		}
	}
	middlewares["box-"+boxID+"-redirect-https"] = map[string]any{"redirectScheme": map[string]any{"scheme": "https", "permanent": true}}
	if request.Desktop != nil {
		desktop := request.Desktop
		password, err := a.desktopPassword(ctx, boxID)
		if err != nil {
			return nil, nil, fmt.Errorf("inspect box credentials: %w", err)
		}
		addHTTPSRoute("box-"+boxID+"-desktop", desktop.Host, "box-"+boxID+"-desktop", true)
		middlewares["box-"+boxID+"-forward-auth"] = forwardAuthMiddleware(a.config.ForwardAuthURL)
		passwordHeader := "Basic " + encodeBasicAuth("abc", password)
		middlewares["box-"+boxID+"-desktop-headers"] = map[string]any{"headers": map[string]any{"customRequestHeaders": map[string]string{"Authorization": passwordHeader}}}
		routers["box-"+boxID+"-desktop"].(map[string]any)["middlewares"] = []string{"box-" + boxID + "-forward-auth", "box-" + boxID + "-desktop-headers"}
		services["box-"+boxID+"-desktop"] = map[string]any{"loadBalancer": map[string]any{"servers": []map[string]string{{"url": "http://{{BOX_IP}}:3000"}}}}
		credentials[desktop.Host] = authCredential{boxID: boxID, hash: desktop.TokenHash, expiresAt: desktop.ExpiresAt}
	}
	for index, host := range request.Hosts {
		name := fmt.Sprintf("box-%s-host-%d", boxID, index)
		service := name
		addHTTPSRoute(name, host.Host, service, host.Private)
		if host.Private {
			middlewares["box-"+boxID+"-forward-auth"] = forwardAuthMiddleware(a.config.ForwardAuthURL)
			credentials[host.Host] = authCredential{boxID: boxID, hash: host.TokenHash}
		}
		services[service] = map[string]any{"loadBalancer": map[string]any{"servers": []map[string]string{{"url": fmt.Sprintf("http://{{BOX_IP}}:%d", host.Port)}}}}
	}
	if len(request.Hosts) != 0 || request.Desktop != nil {
		container, err := a.docker.Inspect(ctx, boxName(boxID))
		if err != nil || container.IP == "" {
			return nil, nil, errors.New("box container has no network address")
		}
		for _, serviceValue := range services {
			service := serviceValue.(map[string]any)
			loadBalancer := service["loadBalancer"].(map[string]any)
			servers := loadBalancer["servers"].([]map[string]string)
			for _, server := range servers {
				server["url"] = strings.ReplaceAll(server["url"], "{{BOX_IP}}", container.IP)
			}
		}
	}
	httpConfig := make(map[string]any)
	if len(routers) != 0 {
		httpConfig["routers"] = routers
	}
	if len(middlewares) != 0 {
		httpConfig["middlewares"] = middlewares
	}
	if len(services) != 0 {
		httpConfig["services"] = services
	}
	content, err := yaml.Marshal(map[string]any{"http": httpConfig})
	if err != nil {
		return nil, nil, err
	}
	return content, credentials, nil
}

func forwardAuthMiddleware(address string) map[string]any {
	return map[string]any{"forwardAuth": map[string]any{
		"address":                  address,
		"trustForwardHeader":       true,
		"addAuthCookiesToResponse": []string{"paddock_auth"},
	}}
}

func encodeBasicAuth(user, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
}

func (a *Agent) desktopPassword(ctx context.Context, boxID string) (string, error) {
	if err := a.loadContainerCredentials(ctx, boxID); err != nil {
		return "", err
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.desktopPasswords[boxID], nil
}

func atomicWrite(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".box-route-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return err
	}
	return nil
}
