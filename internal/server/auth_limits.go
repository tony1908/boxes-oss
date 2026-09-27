package server

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func constantTimeStringMatch(provided, expected string) bool {
	return len(provided) == len(expected) && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func (s *Server) authRateLimited(r *http.Request, email string, checkEmail bool) bool {
	now := s.now()
	s.authLimitMu.Lock()
	defer s.authLimitMu.Unlock()
	for key, times := range s.authIPAttempts {
		if len(times) == 0 || now.Sub(times[len(times)-1]) > time.Hour {
			delete(s.authIPAttempts, key)
		}
	}
	for key, times := range s.authEmailFails {
		if len(times) == 0 || now.Sub(times[len(times)-1]) > time.Hour {
			delete(s.authEmailFails, key)
		}
	}
	if len(s.authIPAttempts) > 10000 {
		evictAuthAttempts(s.authIPAttempts, 10000)
	}
	if len(s.authEmailFails) > 10000 {
		evictAuthAttempts(s.authEmailFails, 10000)
	}
	ip := clientIP(r)
	cutoff := now.Add(-time.Minute)
	times := recentTimes(s.authIPAttempts[ip], cutoff)
	if len(times) >= 10 {
		s.authIPAttempts[ip] = times
		return true
	}
	times = append(times, now)
	s.authIPAttempts[ip] = times
	if checkEmail {
		failures := recentTimes(s.authEmailFails[strings.ToLower(strings.TrimSpace(email))], now.Add(-15*time.Minute))
		return len(failures) >= 5
	}
	return false
}

func (s *Server) authRetryAfter(r *http.Request, email string, checkEmail bool) int {
	now := s.now()
	s.authLimitMu.Lock()
	defer s.authLimitMu.Unlock()
	wait := time.Second
	if items := recentTimes(s.authIPAttempts[clientIP(r)], now.Add(-time.Minute)); len(items) >= 10 {
		wait = items[0].Add(time.Minute).Sub(now)
	}
	if checkEmail {
		if items := recentTimes(s.authEmailFails[strings.ToLower(strings.TrimSpace(email))], now.Add(-15*time.Minute)); len(items) >= 5 {
			if emailWait := items[0].Add(15 * time.Minute).Sub(now); emailWait > wait {
				wait = emailWait
			}
		}
	}
	seconds := int(wait.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

func (s *Server) recordAuthFailure(r *http.Request, email string) {
	key := strings.ToLower(strings.TrimSpace(email))
	if key == "" {
		return
	}
	now := s.now()
	s.authLimitMu.Lock()
	failures := recentTimes(s.authEmailFails[key], now.Add(-15*time.Minute))
	s.authEmailFails[key] = append(failures, now)
	s.authLimitMu.Unlock()
}

func (s *Server) resetAuthFailures(email string) {
	s.authLimitMu.Lock()
	delete(s.authEmailFails, strings.ToLower(strings.TrimSpace(email)))
	s.authLimitMu.Unlock()
}

func recentTimes(items []time.Time, cutoff time.Time) []time.Time {
	first := 0
	for first < len(items) && !items[first].After(cutoff) {
		first++
	}
	return items[first:]
}

func clientIP(r *http.Request) string {
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remote = r.RemoteAddr
	}
	peer := net.ParseIP(remote)
	if peer != nil && peer.IsLoopback() {
		forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		if len(forwarded) > 0 {
			candidate := strings.TrimSpace(forwarded[len(forwarded)-1])
			if ip := net.ParseIP(candidate); ip != nil {
				return ip.String()
			}
		}
	}
	if peer != nil {
		return peer.String()
	}
	return remote
}

func evictAuthAttempts(entries map[string][]time.Time, maximum int) {
	for len(entries) > maximum {
		oldestKey := ""
		var oldest time.Time
		for key, times := range entries {
			if len(times) == 0 || oldestKey == "" || times[len(times)-1].Before(oldest) {
				oldestKey = key
				if len(times) > 0 {
					oldest = times[len(times)-1]
				}
			}
		}
		delete(entries, oldestKey)
	}
}

func writeRateLimited(w http.ResponseWriter, retryAfter int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	writeError(w, http.StatusTooManyRequests, "rate_limited", "too many authentication attempts")
}
