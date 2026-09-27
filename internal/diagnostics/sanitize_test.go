package diagnostics

import (
	"strings"
	"testing"
)

func TestSanitizeRedactsAWSStorageCredentials(t *testing.T) {
	got := Sanitize("AWS_ACCESS_KEY_ID=access-value AWS_SECRET_ACCESS_KEY:secret-value")
	if strings.Contains(got, "access-value") || strings.Contains(got, "secret-value") {
		t.Fatalf("Sanitize() leaked an S3 credential: %q", got)
	}
}
