package boxd

import (
	"context"
	"encoding/json"

	"github.com/boxes-oss/boxes/internal/computer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewMCPServer creates an MCP server exposing the box's desktop controls.
func NewMCPServer(desktop *computer.Computer) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "boxes-computer", Version: "1.0.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: "screenshot", Description: "Capture the current desktop as a PNG image."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			png, err := desktop.Screenshot()
			if err != nil {
				return nil, nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: png, MIMEType: "image/png"}}}, nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "click", Description: "Click a mouse button at screen coordinates."},
		func(_ context.Context, _ *mcp.CallToolRequest, in struct {
			X      int    `json:"x" jsonschema:"horizontal screen coordinate"`
			Y      int    `json:"y" jsonschema:"vertical screen coordinate"`
			Button string `json:"button,omitempty" jsonschema:"mouse button: left, middle, or right"`
			Double bool   `json:"double,omitempty" jsonschema:"whether to click twice"`
		}) (*mcp.CallToolResult, any, error) {
			button := in.Button
			if button == "" {
				button = "left"
			}
			if err := desktop.Click(in.X, in.Y, button, in.Double); err != nil {
				return nil, nil, err
			}
			return toolOK(), nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "move", Description: "Move the mouse pointer to screen coordinates."},
		func(_ context.Context, _ *mcp.CallToolRequest, in struct {
			X int `json:"x" jsonschema:"horizontal screen coordinate"`
			Y int `json:"y" jsonschema:"vertical screen coordinate"`
		}) (*mcp.CallToolResult, any, error) {
			if err := desktop.Move(in.X, in.Y); err != nil {
				return nil, nil, err
			}
			return toolOK(), nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "drag", Description: "Drag the left mouse button between screen coordinates."},
		func(_ context.Context, _ *mcp.CallToolRequest, in struct {
			FromX int `json:"from_x" jsonschema:"starting horizontal screen coordinate"`
			FromY int `json:"from_y" jsonschema:"starting vertical screen coordinate"`
			ToX   int `json:"to_x" jsonschema:"ending horizontal screen coordinate"`
			ToY   int `json:"to_y" jsonschema:"ending vertical screen coordinate"`
		}) (*mcp.CallToolResult, any, error) {
			if err := desktop.Drag(in.FromX, in.FromY, in.ToX, in.ToY); err != nil {
				return nil, nil, err
			}
			return toolOK(), nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "scroll", Description: "Scroll the mouse wheel."},
		func(_ context.Context, _ *mcp.CallToolRequest, in struct {
			Direction string `json:"direction" jsonschema:"scroll direction: up, down, left, or right"`
			Amount    int    `json:"amount" jsonschema:"number of scroll notches"`
		}) (*mcp.CallToolResult, any, error) {
			if err := desktop.Scroll(in.Direction, in.Amount); err != nil {
				return nil, nil, err
			}
			return toolOK(), nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "type", Description: "Type literal text into the focused window."},
		func(_ context.Context, _ *mcp.CallToolRequest, in struct {
			Text string `json:"text" jsonschema:"text to type"`
		}) (*mcp.CallToolResult, any, error) {
			if err := desktop.Type(in.Text); err != nil {
				return nil, nil, err
			}
			return toolOK(), nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "key", Description: "Send a keyboard key or key combination."},
		func(_ context.Context, _ *mcp.CallToolRequest, in struct {
			Combo string `json:"combo" jsonschema:"key or key combination, for example ctrl+shift+t"`
		}) (*mcp.CallToolResult, any, error) {
			if err := desktop.Key(in.Combo); err != nil {
				return nil, nil, err
			}
			return toolOK(), nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "open_url", Description: "Open a URL in the desktop's default browser."},
		func(_ context.Context, _ *mcp.CallToolRequest, in struct {
			URL string `json:"url" jsonschema:"URL to open"`
		}) (*mcp.CallToolResult, any, error) {
			if err := desktop.OpenURL(in.URL); err != nil {
				return nil, nil, err
			}
			return toolOK(), nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "launch_app", Description: "Launch a desktop application."},
		func(_ context.Context, _ *mcp.CallToolRequest, in struct {
			Name string `json:"name" jsonschema:"application name or executable"`
		}) (*mcp.CallToolResult, any, error) {
			if err := desktop.LaunchApp(in.Name); err != nil {
				return nil, nil, err
			}
			return toolOK(), nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "list_windows", Description: "List windows visible on the desktop."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			windows, err := desktop.ListWindows()
			if err != nil {
				return nil, nil, err
			}
			encoded, err := json.Marshal(windows)
			if err != nil {
				return nil, nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}}, nil, nil
		})

	return server
}

// RunMCP serves the desktop-control tools over MCP stdio.
func RunMCP(ctx context.Context, desktop *computer.Computer) error {
	return NewMCPServer(desktop).Run(ctx, &mcp.StdioTransport{})
}

func toolOK() *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "OK"}}}
}
