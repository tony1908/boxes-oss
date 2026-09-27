package diagnostics

import (
	"regexp"
	"strings"
)

var (
	credentialValue = regexp.MustCompile(`(?i)\b(password|token|secret|api[_-]?key|authorization|aws_access_key_id|aws_secret_access_key|aws_session_token)(\s*[:=]\s*|\s+)("[^"]*"|'[^']*'|[^\s,;]+)`)
	bearerValue     = regexp.MustCompile(`(?i)\bBearer\s+[^\s,;]+`)
	urlCredentials  = regexp.MustCompile(`(?i)(https?://)[^/@\s]+:[^/@\s]+@`)
)

// Sanitize removes common credential forms from diagnostic text before it is logged or returned.
func Sanitize(value string) string {
	value = urlCredentials.ReplaceAllString(value, `${1}[REDACTED]@`)
	value = bearerValue.ReplaceAllString(value, "Bearer [REDACTED]")
	value = credentialValue.ReplaceAllStringFunc(value, func(match string) string {
		separator := strings.IndexAny(match, ":=")
		if separator < 0 {
			fields := strings.Fields(match)
			if len(fields) > 0 {
				return fields[0] + " [REDACTED]"
			}
			return "[REDACTED]"
		}
		return match[:separator+1] + "[REDACTED]"
	})
	return strings.TrimSpace(value)
}
