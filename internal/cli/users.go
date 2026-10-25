package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

const usersUsage = "usage: boxes user add <box> <name> | list <box> | rm <box> <name>; boxes group add <box> <group>; boxes group member add|rm <box> <group> <user>"

// user handles `boxes user add|list|rm`: isolated Linux users inside a box.
func (c *CLI) user(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(usersUsage)
	}
	switch args[0] {
	case "add":
		if len(args) != 3 {
			return errors.New(usersUsage)
		}
		id, err := c.boxID(args[1])
		if err != nil {
			return err
		}
		result, err := c.client.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/users", map[string]any{"username": args[2]}, nil)
		if err != nil {
			return err
		}
		return c.printValue(result)
	case "list", "ls":
		if len(args) != 2 {
			return errors.New(usersUsage)
		}
		id, err := c.boxID(args[1])
		if err != nil {
			return err
		}
		result, err := c.client.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id)+"/users", nil, nil)
		if err != nil {
			return err
		}
		return c.printValue(unwrap(result, "users"))
	case "rm", "delete":
		if len(args) != 3 {
			return errors.New(usersUsage)
		}
		id, err := c.boxID(args[1])
		if err != nil {
			return err
		}
		_, err = c.client.Do(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id)+"/users/"+url.PathEscape(args[2]), nil, nil)
		if err == nil && c.jsonOutput {
			return c.printJSON(map[string]any{"ok": true})
		}
		return err
	default:
		return fmt.Errorf("unknown user command %q; %s", args[0], usersUsage)
	}
}

// group handles `boxes group add` and `boxes group member add|rm`: shared groups inside a box.
func (c *CLI) group(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(usersUsage)
	}
	switch args[0] {
	case "add":
		if len(args) != 3 {
			return errors.New(usersUsage)
		}
		id, err := c.boxID(args[1])
		if err != nil {
			return err
		}
		result, err := c.client.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/groups", map[string]any{"name": args[2]}, nil)
		if err != nil {
			return err
		}
		return c.printValue(result)
	case "member":
		if len(args) != 5 || (args[1] != "add" && args[1] != "rm") {
			return errors.New(usersUsage)
		}
		id, err := c.boxID(args[2])
		if err != nil {
			return err
		}
		endpoint := "/sandboxes/" + url.PathEscape(id) + "/groups/" + url.PathEscape(args[3]) + "/members/" + url.PathEscape(args[4])
		if args[1] == "add" {
			result, err := c.client.doJSON(ctx, http.MethodPut, endpoint, nil, nil)
			if err != nil {
				return err
			}
			return c.printValue(result)
		}
		_, err = c.client.Do(ctx, http.MethodDelete, endpoint, nil, nil)
		if err == nil && c.jsonOutput {
			return c.printJSON(map[string]any{"ok": true})
		}
		return err
	default:
		return fmt.Errorf("unknown group command %q; %s", args[0], usersUsage)
	}
}
