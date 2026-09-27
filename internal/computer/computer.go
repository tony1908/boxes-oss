// Package computer provides desktop-control operations for a box.
package computer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const display = "DISPLAY=:1"

// Executor runs an external command without invoking a shell.
type Executor interface {
	Run(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (exitCode int, err error)
}

// Computer executes desktop commands through the supplied executor.
type Computer struct {
	executor Executor
}

// Window describes a window returned by wmctrl.
type Window struct {
	ID      string `json:"id"`
	Desktop int    `json:"desktop"`
	Host    string `json:"host"`
	Title   string `json:"title"`
}

// New creates a computer controller backed by executor.
func New(executor Executor) *Computer {
	return &Computer{executor: executor}
}

// Screenshot captures the current desktop as PNG bytes.
func (c *Computer) Screenshot() ([]byte, error) {
	return c.output([]string{"scrot", "-"})
}

// Click clicks a mouse button at the given screen coordinates.
func (c *Computer) Click(x, y int, button string, double bool) error {
	buttonNumber, err := mouseButton(button)
	if err != nil {
		return err
	}
	argv := []string{"xdotool", "mousemove", strconv.Itoa(x), strconv.Itoa(y), "click"}
	if double {
		argv = append(argv, "--repeat", "2", "--delay", "100")
	}
	argv = append(argv, buttonNumber)
	return c.command(argv...)
}

// Move moves the pointer to the given screen coordinates.
func (c *Computer) Move(x, y int) error {
	return c.command("xdotool", "mousemove", strconv.Itoa(x), strconv.Itoa(y))
}

// Drag holds the left mouse button while moving between two coordinates.
func (c *Computer) Drag(fromX, fromY, toX, toY int) error {
	return c.command("xdotool", "mousemove", strconv.Itoa(fromX), strconv.Itoa(fromY), "mousedown", "1", "mousemove", "--sync", strconv.Itoa(toX), strconv.Itoa(toY), "mouseup", "1")
}

// Scroll scrolls in dir by amount notches.
func (c *Computer) Scroll(dir string, amount int) error {
	if amount <= 0 {
		return fmt.Errorf("scroll amount must be positive")
	}
	button, ok := map[string]string{"up": "4", "down": "5", "left": "6", "right": "7"}[strings.ToLower(dir)]
	if !ok {
		return fmt.Errorf("invalid scroll direction %q", dir)
	}
	return c.command("xdotool", "click", "--repeat", strconv.Itoa(amount), button)
}

// Type types text literally into the focused window.
func (c *Computer) Type(text string) error {
	if strings.ContainsRune(text, '\x00') {
		return fmt.Errorf("text cannot contain NUL")
	}
	return c.command("xdotool", "type", "--clearmodifiers", "--delay", "0", "--", text)
}

// Key sends an xdotool key combination.
func (c *Computer) Key(combo string) error {
	if strings.TrimSpace(combo) == "" || strings.ContainsRune(combo, '\x00') {
		return fmt.Errorf("key combination is required")
	}
	return c.command("xdotool", "key", "--clearmodifiers", combo)
}

// OpenURL opens url in the desktop's default browser.
func (c *Computer) OpenURL(url string) error {
	if url == "" || strings.ContainsRune(url, '\x00') {
		return fmt.Errorf("URL is required")
	}
	return c.displayCommand("xdg-open", url)
}

// LaunchApp starts the named application without a shell.
func (c *Computer) LaunchApp(name string) error {
	if strings.TrimSpace(name) == "" || strings.ContainsRune(name, '\x00') {
		return fmt.Errorf("application name is required")
	}
	return c.displayCommand(name)
}

// ListWindows returns the windows reported by wmctrl.
func (c *Computer) ListWindows() ([]Window, error) {
	output, err := c.output([]string{"wmctrl", "-l"})
	if err != nil {
		return nil, err
	}
	var windows []Window
	for lineNumber, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return nil, fmt.Errorf("parse wmctrl output line %d: expected at least 3 fields", lineNumber+1)
		}
		desktop, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("parse wmctrl output line %d desktop: %w", lineNumber+1, err)
		}
		windows = append(windows, Window{
			ID:      fields[0],
			Desktop: desktop,
			Host:    fields[2],
			Title:   strings.Join(fields[3:], " "),
		})
	}
	return windows, nil
}

func (c *Computer) command(argv ...string) error {
	_, err := c.run(withDisplay(argv), nil, nil)
	return err
}

func (c *Computer) displayCommand(argv ...string) error {
	_, err := c.run(withDisplay(argv), nil, nil)
	return err
}

func (c *Computer) output(argv []string) ([]byte, error) {
	var stdout bytes.Buffer
	if _, err := c.run(withDisplay(argv), &stdout, nil); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

func (c *Computer) run(argv []string, stdout, stderr io.Writer) (int, error) {
	code, err := c.executor.Run(context.Background(), argv, nil, stdout, stderr)
	if err != nil {
		return code, err
	}
	if code != 0 {
		return code, fmt.Errorf("%s exited with code %d", argv[2], code)
	}
	return code, nil
}

func withDisplay(argv []string) []string {
	command := make([]string, 0, len(argv)+2)
	command = append(command, "env", display)
	return append(command, argv...)
}

func mouseButton(button string) (string, error) {
	button = strings.ToLower(button)
	switch button {
	case "left":
		return "1", nil
	case "middle":
		return "2", nil
	case "right":
		return "3", nil
	default:
		return "", fmt.Errorf("invalid mouse button %q", button)
	}
}
