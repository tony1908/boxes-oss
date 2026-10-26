package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const screenshotRemotePath = "/tmp/paddock-screenshot.png"

// screenshot captures the box desktop (display :1) with scrot as the default user, downloads the PNG, and
// deletes the temporary file in the box.
func (c *CLI) screenshot(ctx context.Context, args []string) error {
	usage := errors.New("usage: paddock screenshot <id|current> [-o file.png]")
	if len(args) == 0 {
		return usage
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	dest := fmt.Sprintf("screenshot-%s.png", id)
	for i := 1; i < len(args); i++ {
		if (args[i] == "-o" || args[i] == "--output") && i+1 < len(args) {
			dest = args[i+1]
			i++
			continue
		}
		return usage
	}
	commands := "/sandboxes/" + url.PathEscape(id) + "/commands"
	defer func() {
		_, _ = c.client.doJSON(ctx, http.MethodPost, commands, map[string]any{"cmd": []string{"rm", "-f", screenshotRemotePath}}, nil)
	}()
	result, err := c.client.doJSON(ctx, http.MethodPost, commands, map[string]any{"cmd": []string{"scrot", "-o", screenshotRemotePath}, "desktop": true}, nil)
	if err != nil {
		return err
	}
	var captured struct {
		ExitCode int    `json:"exitCode"`
		Stderr   string `json:"stderr"`
	}
	if err := json.Unmarshal(result, &captured); err != nil {
		return fmt.Errorf("decode command result: %w", err)
	}
	if captured.ExitCode != 0 {
		return fmt.Errorf("capture screenshot failed with status %d: %s", captured.ExitCode, strings.TrimSpace(captured.Stderr))
	}
	if err := c.fetchArtifact(ctx, id, screenshotRemotePath, dest); err != nil {
		return err
	}
	_, err = fmt.Fprintln(c.Out, dest)
	return err
}
