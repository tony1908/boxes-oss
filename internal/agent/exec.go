package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
)

// ExecExecutor runs argv directly without a shell.
type ExecExecutor struct{}

func (ExecExecutor) Run(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(argv) > 0 && (argv[0] == "restic" || (argv[0] == "docker" && containsTar(argv))) {
		if _, err := exec.LookPath("ionice"); err == nil {
			argv = append([]string{"ionice", "-c3", "nice", "-n", "19"}, argv...)
		} else {
			argv = append([]string{"nice", "-n", "19"}, argv...)
		}
	}
	return ExecExecutor{}.RunWithEnv(ctx, argv, stdin, stdout, stderr, nil)
}

func containsTar(argv []string) bool {
	for _, arg := range argv {
		if arg == "tar" {
			return true
		}
	}
	return false
}

func (ExecExecutor) RunWithEnv(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer, env map[string]string) (int, error) {
	if len(argv) == 0 || argv[0] == "" {
		return -1, errors.New("command argv is required")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	if len(env) != 0 {
		merged := make(map[string]string)
		for _, entry := range os.Environ() {
			key, value, ok := strings.Cut(entry, "=")
			if ok {
				merged[key] = value
			}
		}
		for key, value := range env {
			merged[key] = value
		}
		command.Env = make([]string, 0, len(merged))
		for key, value := range merged {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return exitError.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}
