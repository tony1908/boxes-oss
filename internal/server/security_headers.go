package server

import (
	"net/http"
	"net/url"
	"strings"
)

func setSecurityHeaders(w http.ResponseWriter, r *http.Request, publicURL string, nodeHostBases []string) {
	parsed, _ := url.Parse(publicURL)
	host := parsed.Hostname()
	frameHosts := map[string]struct{}{host: {}}
	for _, base := range nodeHostBases {
		if base = sanitizeHostBase(base); base != "" {
			frameHosts[base] = struct{}{}
		}
	}
	frameSrc := make([]string, 0, len(frameHosts))
	for base := range frameHosts {
		if base != "" {
			frameSrc = append(frameSrc, "https://*."+base)
		}
	}
	csp := "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self' https://" + host + " wss://" + host + "; frame-src " + strings.Join(frameSrc, " ") + "; frame-ancestors 'none'"
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if strings.EqualFold(parsed.Scheme, "https") || r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}

func sanitizeHostBase(hostBase string) string {
	hostBase = strings.ToLower(hostBase)
	for i := 0; i < len(hostBase); i++ {
		c := hostBase[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '.' && c != '-' {
			return ""
		}
	}
	return hostBase
}
