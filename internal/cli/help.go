package cli

import (
	"fmt"
	"regexp"
)

// boxIDPattern matches the IDs the server issues (types.NewID("bx_")). Only such IDs become the current box.
var boxIDPattern = regexp.MustCompile(`^bx_[a-z2-7]{12}$`)

// commandUsage is the usage text per command, printed by `boxes <command> --help` and `boxes help <command>`.
var commandUsage = map[string]string{
	"login":         "usage: boxes login --server URL --api-key KEY\n",
	"logout":        "usage: boxes logout\n",
	"whoami":        "usage: boxes whoami\n",
	"current":       "usage: boxes current\n",
	"skill":         "usage: boxes skill install [--claude] [--codex] [--force]\n",
	"new":           "usage: boxes new [--mount DRIVE:PATH[:ro]]... [--type SIZE] [--image REF] [--ttl SECONDS] [--no-auto-stop] [-e KEY=VALUE]... [--setup-file FILE] [--git URL [--revision X] [--depth N] [--git-token-env VAR] [--path P] | --tarball URL [--path P]] [--from NAME] [--environment NAME] [--no-env] [--tag KEY=VALUE]... [--keep-snapshots N] [--network MODE] [--allow-domain HOST]... [--allow-cidr CIDR]... [--deny-cidr CIDR]...\n",
	"list":          "usage: boxes list [--filter r|s|e...] [--all] [--tag KEY=VALUE]... [--json]\n",
	"get":           "usage: boxes get <id>\n",
	"info":          "usage: boxes info <id|current>\n",
	"status":        "usage: boxes status\n",
	"exec":          "usage: boxes exec <id|current> [--user NAME] [--cwd DIR] [--env KEY=VALUE]... [--detach] [--status PID] [--desktop] -- <command> [arguments]\n  --desktop runs the command in the box desktop session (DISPLAY=:1); default user abc only\n",
	"ps":            "usage: boxes ps <id|current>\n",
	"logs":          "usage: boxes logs <id|current> <processId> [-f]\n",
	"kill":          "usage: boxes kill <id|current> <processId> [--signal TERM]\n",
	"ssh":           "usage: boxes ssh <id> [-- command...]\n",
	"proxy":         "usage: boxes proxy <id>\n",
	"scp":           "usage: boxes scp <source> <destination> [arguments]\n",
	"push":          "usage: boxes push <id> [dir] [--to path]\n",
	"pull":          "usage: boxes pull <id> <path> [-o dest]\n",
	"screenshot":    "usage: boxes screenshot <id|current> [-o file.png]\n  captures the box desktop (display :1) and downloads it; default file screenshot-<id>.png\n",
	"prompt":        "usage: boxes prompt [id|current] \"text\" [--provider PROVIDER --model MODEL --new --conversation ID --stop-when-done]\n",
	"events":        "usage: boxes events <id> [--follow] [--after CURSOR]\n",
	"steer":         "usage: boxes steer <id> <message> [--conversation ID]\n",
	"interrupt":     "usage: boxes interrupt <id> [--conversation ID]\n",
	"conversations": "usage: boxes conversations <id|current>\n",
	"snapshots":     "usage: boxes snapshots <id|current>\n       boxes snapshots rm <boxId> <snapshotId>\n",
	"restore":       "usage: boxes restore <id|current> <snapshotId>\n",
	"usage":         "usage: boxes usage <id|current>\n",
	"limits":        "usage: boxes limits\n",
	"desktop":       "usage: boxes desktop <id> [--public] [--vnc] [--profile NAME]\n",
	"browser":       "usage: boxes browser <id> [--public] [--vnc] [--profile NAME]\n",
	"host":          "usage: boxes host <id> <port> [--private]\n",
	"stop":          "usage: boxes stop [id|current]\n",
	"resume":        "usage: boxes resume <id> [--type SIZE]\n",
	"extend":        "usage: boxes extend <id|current> --hours N | --ttl SECONDS | --no-auto-stop\n",
	"update":        "usage: boxes update <id|current> [--tag KEY=VALUE]... [--clear-tags] [--keep-snapshots N] [--network MODE] [--allow-domain HOST]... [--allow-cidr CIDR]... [--deny-cidr CIDR]...\n",
	"fork":          "usage: boxes fork <id|current> [--ttl SECONDS|--no-auto-stop]\n",
	"snapshot":      "usage: boxes snapshot <id> <name> | snapshot pull <snapshotId|boxId> [-o file.tar.gz] | snapshot rm <name>\n",
	"delete":        "usage: boxes delete <id|current> [--yes]\n",
	"dashboard":     "usage: boxes dashboard [id|current]\n",
	"forward":       "usage: boxes forward <id|current> <port> [--local-port N]\n",
	"env":           "usage: boxes env list|create|update|delete|upgrade ...\n",
	"drive":         "usage: boxes drive create <name> | list | rm <name> | fork <source> <target>\n",
	"user":          usersUsage + "\n",
	"group":         usersUsage + "\n",
	"help":          "usage: boxes help [command]\n",
}

// wantsHelp reports whether args ask for help. Arguments after "--" belong to a remote command and are ignored.
func wantsHelp(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

// printCommandUsage prints one command's usage to stdout.
func (c *CLI) printCommandUsage(name string) error {
	usage, ok := commandUsage[name]
	if !ok {
		return fmt.Errorf("unknown command %q; run boxes --help", name)
	}
	_, err := fmt.Fprint(c.Out, usage)
	return err
}
