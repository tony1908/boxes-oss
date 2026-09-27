package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/boxes-oss/boxes/internal/diagnostics"
	"github.com/boxes-oss/boxes/internal/tunnel"
)

type agentClient struct{ http *http.Client }

type agentHealth struct {
	InstallTiming           map[string]float64 `json:"installTiming"`
	CPUs                    int                `json:"cpus"`
	MemoryBytes             int64              `json:"memoryBytes"`
	FreeMemoryBytes         int64              `json:"freeMemoryBytes"`
	MemAvailableBytes       int64              `json:"memAvailableBytes"`
	SwapFreeBytes           int64              `json:"swapFreeBytes"`
	SwapTotalBytes          int64              `json:"swapTotalBytes"`
	MemoryPressureSomeAvg10 float64            `json:"memoryPressureSomeAvg10"`
	DiskFreeBytes           int64              `json:"diskFreeBytes"`
	Runtime                 string             `json:"runtime"`
	Boxes                   []struct {
		BoxID       string `json:"boxId"`
		ContainerID string `json:"containerId"`
		State       string `json:"state"`
	} `json:"boxes"`
}

func newAgentClient(hub *tunnel.Hub, nodeID string) (*agentClient, bool) {
	client, ok := hub.Client(nodeID)
	if !ok {
		return nil, false
	}
	return &agentClient{http: client}, true
}

func (c *agentClient) json(ctx context.Context, method, endpoint string, input, output any) error {
	return c.jsonWithHeaders(ctx, method, endpoint, input, output, nil)
}

func (c *agentClient) rawJSON(ctx context.Context, method, endpoint string, input any) (*http.Response, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://agent"+endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		return nil, fmt.Errorf("agent route update returned HTTP %d", response.StatusCode)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response, nil
}

func (c *agentClient) jsonWithHeaders(ctx context.Context, method, endpoint string, input, output any, headers http.Header) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode agent request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://agent"+endpoint, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&failure)
		failure.Error.Code = diagnostics.Sanitize(failure.Error.Code)
		failure.Error.Message = diagnostics.Sanitize(failure.Error.Message)
		log.Printf("agent request failed method=%s path=%s status=%d code=%s error=%q", method, endpoint, response.StatusCode, failure.Error.Code, failure.Error.Message)
		return &agentStatusError{status: response.StatusCode, code: failure.Error.Code, message: failure.Error.Message}
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(output); err != nil {
		return fmt.Errorf("decode agent response: %w", err)
	}
	return nil
}

func (c *agentClient) raw(ctx context.Context, method, endpoint string, body io.Reader, contentType string) (*http.Response, error) {
	return c.rawWithHeaders(ctx, method, endpoint, body, contentType, nil)
}

func (c *agentClient) rawWithHeaders(ctx context.Context, method, endpoint string, body io.Reader, contentType string, headers http.Header) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, "http://agent"+endpoint, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	return c.http.Do(request)
}

type agentStatusError struct {
	status  int
	code    string
	message string
}

func (e *agentStatusError) Error() string {
	parts := []string{fmt.Sprintf("agent HTTP %d", e.status)}
	if e.code != "" {
		parts = append(parts, e.code)
	}
	if e.message != "" {
		parts = append(parts, e.message)
	}
	return strings.Join(parts, ": ")
}

func (e *agentStatusError) StatusCode() int { return e.status }

func (e *agentStatusError) ErrorCode() string { return e.code }

func agentErrorStatus(err error) int {
	var status interface{ StatusCode() int }
	if errors.As(err, &status) {
		return status.StatusCode()
	}
	return http.StatusBadGateway
}

func agentErrorCode(err error) string {
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		return coded.ErrorCode()
	}
	return ""
}

func agentErrorDetail(err error) string {
	var agentErr *agentStatusError
	if errors.As(err, &agentErr) {
		parts := make([]string, 0, 2)
		if agentErr.code != "" {
			parts = append(parts, diagnostics.Sanitize(agentErr.code))
		}
		if agentErr.message != "" {
			parts = append(parts, diagnostics.Sanitize(agentErr.message))
		}
		return strings.Join(parts, ": ")
	}
	return diagnostics.Sanitize(err.Error())
}

func withAgentError(message string, err error) string {
	var agentErr *agentStatusError
	if !errors.As(err, &agentErr) {
		detail := diagnostics.Sanitize(err.Error())
		if detail == "" {
			return message
		}
		return message + " (" + detail + ")"
	}
	detail := agentErrorDetail(err)
	if detail == "" {
		return message
	}
	return message + " (agent " + detail + ")"
}

func agentPath(parts ...string) string {
	encoded := make([]string, len(parts))
	for i, part := range parts {
		encoded[i] = url.PathEscape(part)
	}
	return "/" + strings.Join(encoded, "/")
}

func agentTimeoutContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 4*time.Minute)
}
