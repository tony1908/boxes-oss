package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client sends requests to the boxes server REST API.
type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

// NewClient constructs an API client. A nil HTTP client uses http.DefaultClient.
func NewClient(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTPClient: httpClient}
}

// Do performs one API request and returns its response body. API errors are
// decoded from the server's documented {"error":{"code","message"}} shape.
func (c *Client) Do(ctx context.Context, method, endpoint string, body any, headers http.Header) ([]byte, error) {
	if c == nil || c.BaseURL == "" {
		return nil, errors.New("server URL is required; run boxes login or set --server")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	endpoint = "/" + strings.TrimLeft(endpoint, "/")
	request, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/api/v1"+endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request %s %s: %w", method, endpoint, err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope struct {
			Error json.RawMessage `json:"error"`
		}
		var apiErr struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &envelope) == nil && len(envelope.Error) > 0 {
			_ = json.Unmarshal(envelope.Error, &apiErr)
		}
		message := apiErr.Message
		if message == "" {
			message = apiErr.Code
		}
		if message == "" {
			message = strings.TrimSpace(string(data))
		}
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return nil, &statusError{code: response.StatusCode, status: response.Status, message: message}
	}
	return data, nil
}

func (c *Client) DoReader(ctx context.Context, method, endpoint string, body io.Reader, contentType string) ([]byte, error) {
	if c == nil || c.BaseURL == "" {
		return nil, errors.New("server URL is required; run boxes login or set --server")
	}
	endpoint = "/" + strings.TrimLeft(endpoint, "/")
	request, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/api/v1"+endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request %s %s: %w", method, endpoint, err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &envelope)
		message := envelope.Error.Message
		if message == "" {
			message = strings.TrimSpace(string(data))
		}
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return nil, &statusError{code: response.StatusCode, status: response.Status, message: message}
	}
	return data, nil
}

// Download streams an API response to dst and reports bytes copied.
func (c *Client) Download(ctx context.Context, endpoint string, dst io.Writer, progress func(int64)) error {
	if c == nil || c.BaseURL == "" {
		return errors.New("server URL is required; run boxes login or set --server")
	}
	endpoint = "/" + strings.TrimLeft(endpoint, "/")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1"+endpoint, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("request GET %s: %w", endpoint, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, err := io.ReadAll(response.Body)
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &envelope)
		message := envelope.Error.Message
		if message == "" {
			message = strings.TrimSpace(string(data))
		}
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return &statusError{code: response.StatusCode, status: response.Status, message: message}
	}
	buffer := make([]byte, 32*1024)
	var total int64
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			written, writeErr := dst.Write(buffer[:n])
			total += int64(written)
			if progress != nil {
				progress(total)
			}
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			if message := response.Trailer.Get("X-Boxes-Snapshot-Error"); message != "" {
				return fmt.Errorf("snapshot download failed: %s", message)
			}
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read response: %w", readErr)
		}
	}
}

func (c *Client) request(ctx context.Context, method, endpoint string, body, result any, headers http.Header) error {
	data, err := c.Do(ctx, method, endpoint, body, headers)
	if err != nil {
		return err
	}
	if result == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("decode server response: %w", err)
	}
	return nil
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, body any, headers http.Header) (json.RawMessage, error) {
	data, err := c.Do(ctx, method, endpoint, body, headers)
	return json.RawMessage(data), err
}

// statusError is a non-2xx API response. Its text is unchanged; callers can inspect the code.
type statusError struct {
	code    int
	status  string
	message string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("server returned %s: %s", e.status, e.message)
}
