// Package dockercli provides a Docker CLI runtime for managed boxes.
package dockercli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	boxtypes "github.com/tony1908/boxes-oss/internal/types"
)

// Executor runs an external command without invoking a shell.
type Executor interface {
	Run(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (exitCode int, err error)
}

// Spec contains the configuration needed to create a box container.
type Spec struct {
	BoxID           string
	Image           string
	Type            string
	Runtime         string
	DesktopPassword string
	BoxdToken       string
	Env             map[string]string
	DiskQuota       bool
	ToolsVolume     string
}

// Container is the Docker state needed by the agent.
type Container struct {
	ID    string
	Name  string
	State string
	IP    string
}

// Change describes a path modified in a container's writable layer.
type Change struct {
	Kind string
	Path string
}

// CommandError reports a failed Docker command with credential values removed.
type CommandError struct {
	ExitCode int
	Stderr   string
}

func (e *CommandError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("docker command failed with exit code %d", e.ExitCode)
	}
	return fmt.Sprintf("docker command failed with exit code %d: %s", e.ExitCode, e.Stderr)
}

// Docker executes Docker CLI operations using the supplied executor.
type Docker struct {
	executor Executor
	toolsMu  sync.Mutex
}

// New creates a Docker CLI runtime backed by executor.
func New(executor Executor) *Docker {
	return &Docker{executor: executor}
}

// CreateArgs returns the argv for creating a managed box container.
func CreateArgs(spec Spec) ([]string, error) {
	if spec.BoxID == "" {
		return nil, fmt.Errorf("box ID is required")
	}
	if spec.Image == "" {
		return nil, fmt.Errorf("box image is required")
	}
	size, err := boxtypes.SizeFor(spec.Type)
	if err != nil {
		return nil, err
	}
	if spec.Runtime == "" {
		return nil, fmt.Errorf("container runtime is required")
	}
	keys := make([]string, 0, len(spec.Env))
	for key := range spec.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return nil, fmt.Errorf("invalid environment variable name %q", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	argv := []string{
		"docker", "create",
		"--name", "box-" + spec.BoxID,
		"--hostname", spec.BoxID,
		"--runtime", spec.Runtime,
		"--label", "boxes.managed=true",
		"--label", "boxes.box-id=" + spec.BoxID,
		"--cpus", strconv.Itoa(size.CPUs),
		"--memory", strconv.FormatInt(size.MemoryBytes, 10),
		"--memory-reservation", strconv.FormatInt(size.MemoryReservationBytes, 10),
		"--pids-limit", "4096",
		"--shm-size", "1g",
		"--network", "boxes-net",
		"--restart", "unless-stopped",
		"-e", "PUID=1000",
		"-e", "PGID=1000",
		"-e", "TZ=Etc/UTC",
		"-e", "PASSWORD=" + spec.DesktopPassword,
		"-e", "BOXD_TOKEN=" + spec.BoxdToken,
	}
	for _, key := range keys {
		argv = append(argv, "-e", key+"="+spec.Env[key])
	}
	if spec.ToolsVolume != "" {
		if strings.ContainsAny(spec.ToolsVolume, ":/,\x00") {
			return nil, fmt.Errorf("invalid tools volume name %q", spec.ToolsVolume)
		}
		argv = append(argv, "--mount", "type=volume,source="+spec.ToolsVolume+",target=/opt/boxes-tools,readonly")
	}
	if spec.DiskQuota {
		argv = append(argv, "--storage-opt", "size="+strconv.FormatInt(size.DiskBytes, 10))
	}
	return append(argv, spec.Image), nil
}

// EnsureToolsVolume pulls a missing tools image, then returns its populated
// read-only volume. An image that remains unavailable disables the optional mount.
func (d *Docker) EnsureToolsVolume(ctx context.Context, image string) (string, bool, error) {
	if image == "" || strings.EqualFold(image, "none") {
		return "", false, nil
	}
	d.toolsMu.Lock()
	defer d.toolsMu.Unlock()

	imageID, code, err := d.runMode(ctx, []string{"docker", "image", "inspect", "--format", "{{.Id}}", image}, nil, nil, nil, nil, false)
	if err != nil {
		return "", false, err
	}
	if code != 0 {
		if err := d.EnsureImage(ctx, image); err != nil {
			return "", false, nil
		}
		imageID, code, err = d.runMode(ctx, []string{"docker", "image", "inspect", "--format", "{{.Id}}", image}, nil, nil, nil, nil, false)
		if err != nil {
			return "", false, err
		}
		if code != 0 {
			return "", false, nil
		}
	}
	digest := strings.TrimPrefix(strings.TrimSpace(imageID), "sha256:")
	if len(digest) < 12 {
		return "", false, fmt.Errorf("docker image %q returned an invalid image ID", image)
	}
	for _, char := range digest[:12] {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return "", false, fmt.Errorf("docker image %q returned an invalid image ID", image)
		}
	}
	volume := "boxes-tools-" + digest[:12]

	_, code, err = d.runMode(ctx, []string{"docker", "volume", "inspect", volume}, nil, nil, nil, nil, false)
	if err != nil {
		return "", false, err
	}
	if code == 0 {
		return volume, true, nil
	}
	if _, _, err := d.run(ctx, []string{"docker", "volume", "create", volume}, nil, nil, nil, nil); err != nil {
		return "", false, err
	}
	if _, _, err := d.run(ctx, []string{"docker", "run", "--rm", "-v", volume + ":/dst", image, "cp", "-a", "/opt/boxes-tools/.", "/dst/"}, nil, nil, nil, nil); err != nil {
		_, _, _ = d.run(ctx, []string{"docker", "volume", "rm", volume}, nil, nil, nil, nil)
		return "", false, err
	}
	return volume, true, nil
}

// EnsureNetwork ensures boxes-net exists with inter-container communication disabled.
func (d *Docker) EnsureNetwork(ctx context.Context) error {
	_, code, err := d.run(ctx, []string{"docker", "network", "inspect", "boxes-net"}, nil, nil, nil, nil)
	if code == 0 {
		return err
	}
	_, _, err = d.run(ctx, []string{"docker", "network", "create", "--driver", "bridge", "--opt", "com.docker.network.bridge.enable_icc=false", "boxes-net"}, nil, nil, nil, nil)
	return err
}

// NetworkSubnet returns the first IPAM subnet of a Docker network.
func (d *Docker) NetworkSubnet(ctx context.Context, name string) (string, error) {
	stdout, _, err := d.run(ctx, []string{"docker", "network", "inspect", name, "--format", "{{(index .IPAM.Config 0).Subnet}}"}, nil, nil, nil, nil)
	if err != nil {
		return "", err
	}
	subnet := strings.TrimSpace(stdout)
	if subnet == "" {
		return "", fmt.Errorf("network %s has no subnet", name)
	}
	return subnet, nil
}

// EnsureImage pulls image only when it is not already present locally.
func (d *Docker) EnsureImage(ctx context.Context, image string) error {
	if image == "" {
		return fmt.Errorf("image is required")
	}
	_, code, err := d.run(ctx, []string{"docker", "image", "inspect", image}, nil, nil, nil, nil)
	if code == 0 {
		return err
	}
	_, _, err = d.run(ctx, []string{"docker", "pull", image}, nil, nil, nil, nil)
	return err
}

// RemoveImage force-removes an image reference.
func (d *Docker) RemoveImage(ctx context.Context, image string) error {
	if image == "" {
		return fmt.Errorf("image is required")
	}
	_, _, err := d.run(ctx, []string{"docker", "rmi", "-f", image}, nil, nil, nil, nil)
	return err
}

// PruneDanglingImages removes dangling image content.
func (d *Docker) PruneDanglingImages(ctx context.Context) error {
	_, _, err := d.run(ctx, []string{"docker", "image", "prune", "-f"}, nil, nil, nil, nil)
	return err
}

// PullImage pulls an image reference unconditionally.
func (d *Docker) PullImage(ctx context.Context, image string) error {
	if image == "" {
		return fmt.Errorf("image is required")
	}
	_, _, err := d.run(ctx, []string{"docker", "pull", image}, nil, nil, nil, nil)
	return err
}

// Create creates a stopped container and returns its Docker ID.
func (d *Docker) Create(ctx context.Context, spec Spec) (string, error) {
	argv, err := CreateArgs(spec)
	if err != nil {
		return "", err
	}
	stdout, _, err := d.run(ctx, argv, nil, nil, nil, credentials(spec))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// Start starts a container.
func (d *Docker) Start(ctx context.Context, name string) error {
	_, _, err := d.run(ctx, []string{"docker", "start", name}, nil, nil, nil, nil)
	return err
}

// Stop stops a container, allowing timeout for graceful shutdown.
func (d *Docker) Stop(ctx context.Context, name string, timeout time.Duration) error {
	if timeout < 0 {
		return fmt.Errorf("stop timeout cannot be negative")
	}
	seconds := int64(timeout / time.Second)
	if timeout%time.Second != 0 {
		seconds++
	}
	_, _, err := d.run(ctx, []string{"docker", "stop", "--time", strconv.FormatInt(seconds, 10), name}, nil, nil, nil, nil)
	return err
}

// Remove removes a container. If force is true, it is stopped first by Docker.
func (d *Docker) Remove(ctx context.Context, name string, force bool) error {
	argv := []string{"docker", "rm"}
	if force {
		argv = append(argv, "--force")
	}
	argv = append(argv, "-v")
	_, _, err := d.run(ctx, append(argv, name), nil, nil, nil, nil)
	return err
}

// Inspect returns the current Docker ID, state, and boxes-net IP address.
func (d *Docker) Inspect(ctx context.Context, name string) (Container, error) {
	stdout, _, err := d.run(ctx, []string{"docker", "inspect", name}, nil, nil, nil, nil)
	if err != nil {
		return Container{}, err
	}
	var results []struct {
		ID    string `json:"Id"`
		Name  string `json:"Name"`
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal([]byte(stdout), &results); err != nil {
		return Container{}, fmt.Errorf("decode docker inspect output: %w", err)
	}
	if len(results) != 1 {
		return Container{}, fmt.Errorf("docker inspect returned %d containers, want 1", len(results))
	}
	container := results[0]
	nameFromInspect := strings.TrimPrefix(container.Name, "/")
	return Container{ID: container.ID, Name: nameFromInspect, State: container.State.Status, IP: container.NetworkSettings.Networks["boxes-net"].IPAddress}, nil
}

// CPUPercent returns the current container CPU use as a percentage of one core.
func (d *Docker) CPUPercent(ctx context.Context, name string) (float64, error) {
	stdout, _, err := d.run(ctx, []string{"docker", "stats", "--no-stream", "--format", "{{json .}}", name}, nil, nil, nil, nil)
	if err != nil {
		return 0, err
	}
	var stats struct {
		CPU string `json:"CPUPerc"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &stats); err != nil {
		return 0, fmt.Errorf("decode docker stats output: %w", err)
	}
	return strconv.ParseFloat(strings.TrimSuffix(stats.CPU, "%"), 64)
}

// ContainerIDAndPID returns the full container ID and the PID of its init process on the host.
func (d *Docker) ContainerIDAndPID(ctx context.Context, name string) (string, int, error) {
	stdout, _, err := d.run(ctx, []string{"docker", "inspect", "--format", "{{.Id}} {{.State.Pid}}", name}, nil, nil, nil, nil)
	if err != nil {
		return "", 0, err
	}
	id, pidText, ok := strings.Cut(strings.TrimSpace(stdout), " ")
	if !ok {
		return "", 0, fmt.Errorf("decode docker inspect id and pid output %q", stdout)
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 || id == "" {
		return "", 0, fmt.Errorf("decode docker inspect pid %q", pidText)
	}
	return id, pid, nil
}

// DesktopConnections returns the number of established connections to the
// desktop stream ports inside a box.
func (d *Docker) DesktopConnections(ctx context.Context, name string) (int, error) {
	var output bytes.Buffer
	_, err := d.Exec(ctx, name, "root", "", nil, []string{"cat", "/proc/net/tcp", "/proc/net/tcp6"}, nil, &output, io.Discard)
	if err != nil {
		return 0, err
	}
	return countDesktopConnections(output.String()), nil
}

func countDesktopConnections(procNet string) int {
	count := 0
	for _, line := range strings.Split(procNet, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] == "sl" || fields[3] != "01" {
			continue
		}
		_, portHex, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseUint(portHex, 16, 16)
		if err != nil || (port != 3000 && port != 3001) {
			continue
		}
		count++
	}
	return count
}

// InspectEnv returns a container's configured environment variables.
func (d *Docker) InspectEnv(ctx context.Context, name string) (map[string]string, error) {
	stdout, _, err := d.run(ctx, []string{"docker", "inspect", "--format", "{{json .Config.Env}}", name}, nil, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	var entries []string
	if err := json.Unmarshal([]byte(stdout), &entries); err != nil {
		return nil, fmt.Errorf("decode docker inspect environment output: %w", err)
	}
	environment := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid docker inspect environment entry %q", entry)
		}
		environment[key] = value
	}
	return environment, nil
}

// List returns containers matching a Docker label filter.
func (d *Docker) List(ctx context.Context, label string) ([]Container, error) {
	stdout, _, err := d.run(ctx, []string{"docker", "ps", "--all", "--filter", "label=" + label, "--format", "{{.Names}}"}, nil, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []Container{}, nil
	}
	containers := make([]Container, 0, len(lines))
	for _, name := range lines {
		container, err := d.Inspect(ctx, name)
		if err != nil {
			return nil, err
		}
		containers = append(containers, container)
	}
	return containers, nil
}

// Exec executes argv directly in a container without a shell.
func (d *Docker) Exec(ctx context.Context, name, user, cwd string, env map[string]string, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(argv) == 0 {
		return 0, fmt.Errorf("exec argv cannot be empty")
	}
	command := []string{"docker", "exec"}
	if stdin != nil {
		command = append(command, "--interactive")
	}
	if user == "" || user == "abc" {
		command = append(command, "--user", "1000:1000")
	} else {
		command = append(command, "--user", user)
	}
	if cwd != "" {
		command = append(command, "--workdir", cwd)
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return 0, fmt.Errorf("invalid environment variable name %q", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		command = append(command, "--env", key+"="+env[key])
	}
	if user == "" || user == "abc" {
		command = append(command, "--env", "HOME=/config", "--env", "USER=abc")
	}
	command = append(command, name)
	command = append(command, argv...)
	// Output goes straight to the caller: runMode would keep a full copy, which grows without bound for long-running commands.
	var stderrHead boundedBuffer
	stderrHead.limit = 64 << 10
	var errOut io.Writer = &stderrHead
	if stderr != nil {
		errOut = io.MultiWriter(&stderrHead, stderr)
	}
	out := stdout
	if out == nil {
		out = io.Discard
	}
	code, runErr := d.executor.Run(ctx, command, stdin, out, errOut)
	if runErr != nil {
		return code, &CommandError{ExitCode: code, Stderr: redact(stderrHead.String(), envCredentials(env))}
	}
	return code, nil
}

// boundedBuffer keeps the first limit bytes, enough for error messages without growing with output.
type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.limit - b.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

// Diff returns Docker's added, changed, and deleted paths for a container.
func (d *Docker) Diff(ctx context.Context, name string) ([]Change, error) {
	stdout, _, err := d.run(ctx, []string{"docker", "diff", name}, nil, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	var changes []Change
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		kind, path, ok := strings.Cut(line, " ")
		if !ok || path == "" || (kind != "A" && kind != "C" && kind != "D") {
			return nil, fmt.Errorf("invalid docker diff line %q", line)
		}
		changes = append(changes, Change{Kind: kind, Path: path})
	}
	return changes, nil
}

// CopyIn copies a tar archive from stdin into a container path.
func (d *Docker) CopyIn(ctx context.Context, name, dst string, tarReader io.Reader) error {
	_, _, err := d.run(ctx, []string{"docker", "cp", "-a", "-", name + ":" + dst}, tarReader, nil, nil, nil)
	return err
}

func (d *Docker) run(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer, secrets []string) (string, int, error) {
	return d.runMode(ctx, argv, stdin, stdout, stderr, secrets, true)
}

func (d *Docker) runMode(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer, secrets []string, checkExit bool) (string, int, error) {
	var output, errorsOutput bytes.Buffer
	var out io.Writer = &output
	var errOut io.Writer = &errorsOutput
	if stdout != nil {
		out = io.MultiWriter(&output, stdout)
	}
	if stderr != nil {
		errOut = io.MultiWriter(&errorsOutput, stderr)
	}
	code, runErr := d.executor.Run(ctx, argv, stdin, out, errOut)
	if runErr != nil || (checkExit && code != 0) {
		stderrText := redact(errorsOutput.String(), secrets)
		return output.String(), code, &CommandError{ExitCode: code, Stderr: stderrText}
	}
	return output.String(), code, nil
}

func credentials(spec Spec) []string {
	secrets := []string{spec.DesktopPassword, spec.BoxdToken}
	return append(secrets, envCredentials(spec.Env)...)
}

func envCredentials(env map[string]string) []string {
	var secrets []string
	for _, value := range env {
		if value != "" {
			secrets = append(secrets, value)
		}
	}
	return secrets
}

func redact(stderr string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			stderr = strings.ReplaceAll(stderr, secret, "[REDACTED]")
		}
	}
	for _, key := range []string{"PASSWORD", "BOXD_TOKEN"} {
		prefix := key + "="
		for start := 0; ; {
			relative := strings.Index(stderr[start:], prefix)
			if relative < 0 {
				break
			}
			valueStart := start + relative + len(prefix)
			valueEnd := valueStart
			for valueEnd < len(stderr) && !strings.ContainsRune(" \t\r\n'\"", rune(stderr[valueEnd])) {
				valueEnd++
			}
			stderr = stderr[:valueStart] + "[REDACTED]" + stderr[valueEnd:]
			start = valueStart + len("[REDACTED]")
		}
	}
	return stderr
}
