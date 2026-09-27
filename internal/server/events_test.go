package server

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

func TestEventPumpErrorsAreLoggedAtMostOncePerInterval(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	srv := &Server{eventPumpLogAt: make(map[string]time.Time)}

	srv.logEventPumpError("bx_events123456", errors.New("upstream unavailable"))
	srv.logEventPumpError("bx_events123456", errors.New("upstream unavailable"))
	srv.logEventPumpError("bx_other123456", errors.New("upstream unavailable"))

	got := output.String()
	if count := strings.Count(got, "event pump for"); count != 2 {
		t.Fatalf("event pump log count = %d, want one log per box during the rate-limit interval: %s", count, got)
	}
	if !strings.Contains(got, "bx_events123456") || !strings.Contains(got, "upstream unavailable") {
		t.Fatalf("event pump error was not logged with box and cause: %s", got)
	}
}
