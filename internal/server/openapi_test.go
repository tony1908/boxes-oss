package server

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAPIRoutesMatchPublicMuxRoutes(t *testing.T) {
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	routeSource := string(source)
	start := strings.Index(routeSource, "func (s *Server) routes() http.Handler {")
	if start < 0 {
		t.Fatal("routes mux declaration not found")
	}
	end := strings.Index(routeSource[start:], "\n}\n")
	if end < 0 {
		t.Fatal("routes mux body end not found")
	}
	routeSource = routeSource[start : start+end]

	registered := map[string]bool{}
	registration := regexp.MustCompile(`mux\.Handle(?:Func)?\("([^"]+)"`)
	for _, match := range registration.FindAllStringSubmatch(routeSource, -1) {
		pattern := match[1]
		if pattern == "/" {
			continue
		}
		pattern = strings.ReplaceAll(pattern, "{path...}", "{path}")
		fields := strings.SplitN(pattern, " ", 2)
		if len(fields) == 2 {
			registered[fields[0]+" "+fields[1]] = true
			continue
		}
		// proxyBoxd accepts the forwarded HTTP method, so document its common API methods.
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
			registered[method+" "+pattern] = true
		}
	}

	// These routes support installation or internal agent workflows rather than the public REST API.
	excluded := map[string]bool{
		"GET /agent/connect":           true,
		"GET /provider-models":         true,
		"GET /install/cli.ps1":         true,
		"GET /install/cli/{binary}":    true,
	}
	for route := range excluded {
		delete(registered, route)
	}

	specBytes, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(specBytes, &document); err != nil {
		t.Fatalf("parse api/openapi.yaml: %v", err)
	}
	documented := map[string]bool{}
	for path, operations := range document.Paths {
		for method := range operations {
			normalized := strings.ToUpper(method) + " " + path
			if method == "parameters" || method == "servers" || excluded[normalized] {
				continue
			}
			documented[normalized] = true
		}
	}

	var missing, stale []string
	for route := range registered {
		if !documented[route] {
			missing = append(missing, route)
		}
	}
	for route := range documented {
		if !registered[route] {
			stale = append(stale, route)
		}
	}
	if len(missing) > 0 || len(stale) > 0 {
		t.Fatalf("OpenAPI routes differ from the public mux; missing from spec: %v; missing from mux: %v", missing, stale)
	}
}
