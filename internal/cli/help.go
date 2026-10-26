package cli

import (
	"fmt"
	"regexp"
)

// boxIDPattern matches the IDs the server issues (types.NewID("bx_")). Only such IDs become the current box.
var boxIDPattern = regexp.MustCompile(`^bx_[a-z2-7]{12}$`)

// commandUsage is the usage text per command, printed by `paddock <command> --help` and `paddock help <command>`.
var commandUsage = map[string]string{
	"login":         "usage: paddock login --server URL --api-key KEY\n",
	"logout":        "usage: paddock logout\n",
	"whoami":        "usage: paddock whoami\n",
	"current":       "usage: paddock current\n",
	"skill":         "usage: paddock skill install [--claude] [--codex] [--force]\n",
	"new":           "usage: paddock new [--mount DRIVE:PATH[:ro]]... [--type SIZE] [--image REF] [--ttl SECONDS] [--no-auto-stop] [-e KEY=VALUE]... [--setup-file FILE] [--git URL [--revision X] [--depth N] [--git-token-env VAR] [--path P] | --tarball URL [--path P]] [--from NAME] [--environment NAME] [--no-env] [--tag KEY=VALUE]... [--keep-snapshots N] [--network MODE] [--allow-domain HOST]... [--allow-cidr CIDR]... [--deny-cidr CIDR]...\n",
	"list":          "usage: paddock list [--filter r|s|e...] [--all] [--tag KEY=VALUE]... [--json]\n",
	"get":           "usage: paddock get <id>\n",
	"info":          "usage: paddock info <id|current>\n",
	"status":        "usage: paddock status\n",
	"exec":          "usage: paddock exec <id|current> [--user NAME] [--cwd DIR] [--env KEY=VALUE]... [--detach] [--status PID] [--desktop] -- <command> [arguments]\n  --desktop runs the command in the box desktop session (DISPLAY=:1); default user abc only\n",
	"ps":            "usage: paddock ps <id|current>\n",
	"logs":          "usage: paddock logs <id|current> <processId> [-f]\n",
	"kill":          "usage: paddock kill <id|current> <processId> [--signal TERM]\n",
	"ssh":           "usage: paddock ssh <id> [-- command...]\n",
	"proxy":         "usage: paddock proxy <id>\n",
	"scp":           "usage: paddock scp <source> <destination> [arguments]\n",
	"push":          "usage: paddock push <id> [dir] [--to path]\n",
	"pull":          "usage: paddock pull <id> <path> [-o dest]\n",
	"screenshot":    "usage: paddock screenshot <id|current> [-o file.png]\n  captures the box desktop (display :1) and downloads it; default file screenshot-<id>.png\n",
	"prompt":        "usage: paddock prompt [id|current] \"text\" [--provider PROVIDER --model MODEL --new --conversation ID --stop-when-done]\n",
	"events":        "usage: paddock events <id> [--follow] [--after CURSOR]\n",
	"steer":         "usage: paddock steer <id> <message> [--conversation ID]\n",
	"interrupt":     "usage: paddock interrupt <id> [--conversation ID]\n",
	"conversations": "usage: paddock conversations <id|current>\n",
	"snapshots":     "usage: paddock snapshots <id|current>\n       paddock snapshots rm <boxId> <snapshotId>\n",
	"restore":       "usage: paddock restore <id|current> <snapshotId>\n",
	"usage":         "usage: paddock usage <id|current>\n",
	"limits":        "usage: paddock limits\n",
	"desktop":       "usage: paddock desktop <id> [--public] [--vnc] [--profile NAME]\n",
	"browser":       "usage: paddock browser <id> [--public] [--vnc] [--profile NAME]\n",
	"host":          "usage: paddock host <id> <port> [--private]\n",
	"stop":          "usage: paddock stop [id|current]\n",
	"resume":        "usage: paddock resume <id> [--type SIZE]\n",
	"extend":        "usage: paddock extend <id|current> --hours N | --ttl SECONDS | --no-auto-stop\n",
	"update":        "usage: paddock update <id|current> [--tag KEY=VALUE]... [--clear-tags] [--keep-snapshots N] [--network MODE] [--allow-domain HOST]... [--allow-cidr CIDR]... [--deny-cidr CIDR]...\n",
	"fork":          "usage: paddock fork <id|current> [--ttl SECONDS|--no-auto-stop]\n",
	"snapshot":      "usage: paddock snapshot <id> <name> | snapshot pull <snapshotId|boxId> [-o file.tar.gz] | snapshot rm <name>\n",
	"delete":        "usage: paddock delete <id|current> [--yes]\n",
	"dashboard":     "usage: paddock dashboard [id|current]\n",
	"forward":       "usage: paddock forward <id|current> <port> [--local-port N]\n",
	"env":           "usage: paddock env list|create|update|delete|upgrade ...\n",
	"drive":         "usage: paddock drive create <name> | list | rm <name> | fork <source> <target>\n",
	"user":          usersUsage + "\n",
	"group":         usersUsage + "\n",
	"help":          "usage: paddock help [command]\n",
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
		return fmt.Errorf("unknown command %q; run paddock --help", name)
	}
	_, err := fmt.Fprint(c.Out, usage)
	return err
}
