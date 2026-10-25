package dockercli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

type invocation struct {
	argv   []string
	stdin  string
	stdout io.Writer
	stderr io.Writer
}

type scriptedExecutor struct {
	responses []struct {
		code   int
		err    error
		stdout string
		stderr string
	}
	calls []invocation
}

func (e *scriptedExecutor) Run(_ context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	call := invocation{argv: append([]string(nil), argv...), stdout: stdout, stderr: stderr}
	if stdin != nil {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return 0, err
		}
		call.stdin = string(data)
	}
	e.calls = append(e.calls, call)
	if len(e.responses) == 0 {
		return 0, nil
	}
	response := e.responses[0]
	e.responses = e.responses[1:]
	if stdout != nil {
		_, _ = io.WriteString(stdout, response.stdout)
	}
	if stderr != nil {
		_, _ = io.WriteString(stderr, response.stderr)
	}
	return response.code, response.err
}

func TestCreateArgsIncludesManagedContainerLimitsAndSortedEnvironment(t *testing.T) {
	got, err := CreateArgs(Spec{
		BoxID: "bx_abcdefghijkl", Image: "boxes/box:dev", Type: "small", Runtime: "sysbox-runc",
		DesktopPassword: "desktop-secret", BoxdToken: "agent-secret", DiskQuota: true,
		Env: map[string]string{"ZED": "last", "ALPHA": "first"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"docker", "create", "--name", "box-bx_abcdefghijkl", "--hostname", "bx_abcdefghijkl",
		"--runtime", "sysbox-runc", "--label", "boxes.managed=true", "--label", "boxes.box-id=bx_abcdefghijkl",
		"--cpus", "2", "--memory", "4294967296", "--memory-reservation", "268435456", "--pids-limit", "4096", "--shm-size", "1g",
		"--network", "boxes-net", "--restart", "unless-stopped", "-e", "PUID=1000", "-e", "PGID=1000",
		"-e", "TZ=Etc/UTC", "-e", "PASSWORD=desktop-secret", "-e", "BOXD_TOKEN=agent-secret",
		"-e", "ALPHA=first", "-e", "ZED=last", "--storage-opt", "size=12884901888", "boxes/box:dev",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CreateArgs() = %#v, want %#v", got, want)
	}
}

func TestCreateArgsUsesXsmallLimitsAndReadOnlyToolsVolume(t *testing.T) {
	got, err := CreateArgs(Spec{BoxID: "bx_abcdefghijkl", Image: "boxes/box:dev", Type: "xsmall", Runtime: "runc", ToolsVolume: "boxes-tools-abcdef123456"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"docker", "create", "--name", "box-bx_abcdefghijkl", "--hostname", "bx_abcdefghijkl",
		"--runtime", "runc", "--label", "boxes.managed=true", "--label", "boxes.box-id=bx_abcdefghijkl",
		"--cpus", "1", "--memory", "2147483648", "--memory-reservation", "134217728",
		"--pids-limit", "4096", "--shm-size", "1g", "--network", "boxes-net", "--restart", "unless-stopped",
		"-e", "PUID=1000", "-e", "PGID=1000", "-e", "TZ=Etc/UTC", "-e", "PASSWORD=", "-e", "BOXD_TOKEN=",
		"--mount", "type=volume,source=boxes-tools-abcdef123456,target=/opt/boxes-tools,readonly", "boxes/box:dev",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CreateArgs() = %#v, want %#v", got, want)
	}
}

func TestCreateArgsRejectsUnknownSizeAndInvalidEnvironmentKey(t *testing.T) {
	if _, err := CreateArgs(Spec{BoxID: "bx_abcdefghijkl", Image: "image", Type: "tiny"}); err == nil {
		t.Fatal("CreateArgs accepted an unknown box size")
	}
	if _, err := CreateArgs(Spec{BoxID: "bx_abcdefghijkl", Image: "image", Type: "small", Env: map[string]string{"BAD=KEY": "value"}}); err == nil {
		t.Fatal("CreateArgs accepted an environment key that cannot be represented in argv")
	}
}

func TestEnsureNetworkCreatesNetworkWhenMissing(t *testing.T) {
	exec := &scriptedExecutor{responses: []struct {
		code   int
		err    error
		stdout string
		stderr string
	}{{code: 1, stderr: "Error: No such network: boxes-net"}, {}}}
	docker := New(exec)
	if err := docker.EnsureNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"docker", "network", "inspect", "boxes-net"},
		{"docker", "network", "create", "--driver", "bridge", "--opt", "com.docker.network.bridge.enable_icc=false", "boxes-net"},
	}
	if len(exec.calls) != 2 || !reflect.DeepEqual(exec.calls[0].argv, want[0]) || !reflect.DeepEqual(exec.calls[1].argv, want[1]) {
		t.Fatalf("network calls = %#v, want %#v", exec.calls, want)
	}
}

func TestEnsureImageInspectsBeforePulling(t *testing.T) {
	image := "images.example.test/boxes/box:dev"
	exec := &scriptedExecutor{responses: []struct {
		code   int
		err    error
		stdout string
		stderr string
	}{{code: 1, stderr: "not found"}, {}}}
	if err := New(exec).EnsureImage(context.Background(), image); err != nil {
		t.Fatal(err)
	}
	if len(exec.calls) != 2 || !reflect.DeepEqual(exec.calls[0].argv, []string{"docker", "image", "inspect", image}) || !reflect.DeepEqual(exec.calls[1].argv, []string{"docker", "pull", image}) {
		t.Fatalf("image calls = %#v", exec.calls)
	}
}

func TestEnsureToolsVolumePopulatesOncePerImageDigest(t *testing.T) {
	exec := &scriptedExecutor{responses: []struct {
		code   int
		err    error
		stdout string
		stderr string
	}{
		{stdout: "sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"},
		{code: 1, stderr: "not found"},
		{},
		{},
		{stdout: "sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"},
		{},
	}}
	docker := New(exec)
	for i := 0; i < 2; i++ {
		volume, available, err := docker.EnsureToolsVolume(context.Background(), "boxes/tools:latest")
		if err != nil || !available || volume != "boxes-tools-abcdef123456" {
			t.Fatalf("EnsureToolsVolume() = %q, %v, %v", volume, available, err)
		}
	}
	want := [][]string{
		{"docker", "image", "inspect", "--format", "{{.Id}}", "boxes/tools:latest"},
		{"docker", "volume", "inspect", "boxes-tools-abcdef123456"},
		{"docker", "volume", "create", "boxes-tools-abcdef123456"},
		{"docker", "run", "--rm", "-v", "boxes-tools-abcdef123456:/dst", "boxes/tools:latest", "cp", "-a", "/opt/boxes-tools/.", "/dst/"},
		{"docker", "image", "inspect", "--format", "{{.Id}}", "boxes/tools:latest"},
		{"docker", "volume", "inspect", "boxes-tools-abcdef123456"},
	}
	if len(exec.calls) != len(want) {
		t.Fatalf("EnsureToolsVolume() made %d calls, want %d: %#v", len(exec.calls), len(want), exec.calls)
	}
	for i := range want {
		if !reflect.DeepEqual(exec.calls[i].argv, want[i]) {
			t.Errorf("Docker call %d = %#v, want %#v", i, exec.calls[i].argv, want[i])
		}
	}
}

func TestEnsureToolsVolumePullsImageBeforePopulating(t *testing.T) {
	image := "images.example.test/boxes/tools:v7"
	exec := &scriptedExecutor{responses: []struct {
		code   int
		err    error
		stdout string
		stderr string
	}{
		{code: 1, stderr: "not found"},
		{code: 1, stderr: "not found"},
		{},
		{stdout: "sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"},
		{code: 1, stderr: "not found"},
		{},
		{},
	}}
	volume, available, err := New(exec).EnsureToolsVolume(context.Background(), image)
	if err != nil || !available || volume != "boxes-tools-abcdef123456" {
		t.Fatalf("EnsureToolsVolume() = %q, %v, %v", volume, available, err)
	}
	want := [][]string{
		{"docker", "image", "inspect", "--format", "{{.Id}}", image},
		{"docker", "image", "inspect", image},
		{"docker", "pull", image},
		{"docker", "image", "inspect", "--format", "{{.Id}}", image},
		{"docker", "volume", "inspect", "boxes-tools-abcdef123456"},
		{"docker", "volume", "create", "boxes-tools-abcdef123456"},
		{"docker", "run", "--rm", "-v", "boxes-tools-abcdef123456:/dst", image, "cp", "-a", "/opt/boxes-tools/.", "/dst/"},
	}
	if len(exec.calls) != len(want) {
		t.Fatalf("Docker calls = %#v, want %d calls", exec.calls, len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(exec.calls[i].argv, want[i]) {
			t.Errorf("Docker call %d = %#v, want %#v", i, exec.calls[i].argv, want[i])
		}
	}
}

func TestEnsureToolsVolumeSkipsUnsetNoneAndMissingImage(t *testing.T) {
	exec := &scriptedExecutor{responses: []struct {
		code   int
		err    error
		stdout string
		stderr string
	}{{code: 1, stderr: "not found"}, {code: 1, stderr: "not found"}, {code: 1, stderr: "unauthorized"}}}
	docker := New(exec)
	for _, image := range []string{"", "none"} {
		if volume, available, err := docker.EnsureToolsVolume(context.Background(), image); err != nil || available || volume != "" {
			t.Fatalf("EnsureToolsVolume(%q) = %q, %v, %v; want disabled", image, volume, available, err)
		}
	}
	if volume, available, err := docker.EnsureToolsVolume(context.Background(), "missing/tools:latest"); err != nil || available || volume != "" {
		t.Fatalf("EnsureToolsVolume(missing) = %q, %v, %v; want disabled", volume, available, err)
	}
	want := [][]string{
		{"docker", "image", "inspect", "--format", "{{.Id}}", "missing/tools:latest"},
		{"docker", "image", "inspect", "missing/tools:latest"},
		{"docker", "pull", "missing/tools:latest"},
	}
	if len(exec.calls) != len(want) {
		t.Fatalf("Docker calls = %#v, want only the tools image pull attempt", exec.calls)
	}
	for i := range want {
		if !reflect.DeepEqual(exec.calls[i].argv, want[i]) {
			t.Errorf("Docker call %d = %#v, want %#v", i, exec.calls[i].argv, want[i])
		}
	}
}

func TestCreateStartStopAndRemoveBuildArgv(t *testing.T) {
	exec := &scriptedExecutor{}
	docker := New(exec)
	spec := Spec{BoxID: "bx_abcdefghijkl", Image: "image:tag", Type: "small", Runtime: "runc"}
	if _, err := docker.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if err := docker.Start(context.Background(), "box-bx_abcdefghijkl"); err != nil {
		t.Fatal(err)
	}
	if err := docker.Stop(context.Background(), "box-bx_abcdefghijkl", 17*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := docker.Remove(context.Background(), "box-bx_abcdefghijkl", true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(exec.calls[1].argv, []string{"docker", "start", "box-bx_abcdefghijkl"}) ||
		!reflect.DeepEqual(exec.calls[2].argv, []string{"docker", "stop", "--time", "17", "box-bx_abcdefghijkl"}) ||
		!reflect.DeepEqual(exec.calls[3].argv, []string{"docker", "rm", "--force", "-v", "box-bx_abcdefghijkl"}) {
		t.Fatalf("lifecycle calls = %#v", exec.calls[1:])
	}
}

func TestInspectAndListParseContainerInformation(t *testing.T) {
	exec := &scriptedExecutor{responses: []struct {
		code   int
		err    error
		stdout string
		stderr string
	}{
		{stdout: `[{"Id":"container-123","State":{"Status":"running"},"NetworkSettings":{"Networks":{"boxes-net":{"IPAddress":"172.20.0.4"}}}}]`},
		{stdout: "box-bx_abcdefghijkl\n"},
		{stdout: `[{"Id":"container-123","Name":"/box-bx_abcdefghijkl","State":{"Status":"running"},"NetworkSettings":{"Networks":{"boxes-net":{"IPAddress":"172.20.0.4"}}}}]`},
	}}
	docker := New(exec)
	container, err := docker.Inspect(context.Background(), "box-bx_abcdefghijkl")
	if err != nil {
		t.Fatal(err)
	}
	if container.ID != "container-123" || container.State != "running" || container.IP != "172.20.0.4" {
		t.Fatalf("Inspect() = %#v", container)
	}
	containers, err := docker.List(context.Background(), "boxes.managed=true")
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || containers[0].Name != "box-bx_abcdefghijkl" || containers[0].ID != "container-123" || containers[0].State != "running" || containers[0].IP != "172.20.0.4" {
		t.Fatalf("List() = %#v", containers)
	}
}

func TestExecPassesArgvAndStreamsWithoutShell(t *testing.T) {
	exec := &scriptedExecutor{responses: []struct {
		code   int
		err    error
		stdout string
		stderr string
	}{{code: 7, stdout: "output", stderr: "problem"}}}
	var stdout, stderr bytes.Buffer
	code, err := New(exec).Exec(context.Background(), "box-bx_abcdefghijkl", "abc", "/config/work space", map[string]string{"A": "1"}, []string{"printf", "%s", "hello world"}, strings.NewReader("input"), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docker", "exec", "--interactive", "--user", "1000:1000", "--workdir", "/config/work space", "--env", "A=1", "--env", "HOME=/config", "--env", "USER=abc", "box-bx_abcdefghijkl", "printf", "%s", "hello world"}
	if code != 7 || !reflect.DeepEqual(exec.calls[0].argv, want) || exec.calls[0].stdin != "input" || stdout.String() != "output" || stderr.String() != "problem" {
		t.Fatalf("Exec() = code %d, argv %#v, stdin %q, stdout %q, stderr %q", code, exec.calls[0].argv, exec.calls[0].stdin, stdout.String(), stderr.String())
	}
}

func TestDesktopConnectionsReadsProcNetTCPFiles(t *testing.T) {
	procNet := `  sl  local_address rem_address   st
   0: 0100007F:0BB8 0100007F:1234 01 00000000:00000000 00:00000000 00000000 1000 0 1
   1: 0100007F:0BB9 0100007F:1234 01 00000000:00000000 00:00000000 00000000 1000 0 2
   2: 0100007F:0BB8 0100007F:1234 02 00000000:00000000 00:00000000 00000000 1000 0 3
   3: 0100007F:0050 0100007F:1234 01 00000000:00000000 00:00000000 00000000 1000 0 4
  000: 00000000000000000000000001000000:0BB8 00000000000000000000000001000000:1234 01 00000000:00000000 00:00000000 00000000 1000 0 5
  001: 00000000000000000000000001000000:0BB9 00000000000000000000000001000000:1234 02 00000000:00000000 00:00000000 00000000 1000 0 6
`
	if got := countDesktopConnections(procNet); got != 3 {
		t.Fatalf("countDesktopConnections() = %d, want 3", got)
	}
}

func TestDesktopConnectionsExecutesCatInContainer(t *testing.T) {
	exec := &scriptedExecutor{responses: []struct {
		code   int
		err    error
		stdout string
		stderr string
	}{{stdout: "  0: 0100007F:0BB8 0100007F:1234 01\n"}}}
	count, err := New(exec).DesktopConnections(context.Background(), "box-bx_abcdefghijkl")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docker", "exec", "--user", "root", "box-bx_abcdefghijkl", "cat", "/proc/net/tcp", "/proc/net/tcp6"}
	if count != 1 || !reflect.DeepEqual(exec.calls[0].argv, want) {
		t.Fatalf("DesktopConnections() = %d, argv %#v; want 1, %#v", count, exec.calls[0].argv, want)
	}
}

func TestExecTranslatesDefaultUserAndPreservesOtherUsers(t *testing.T) {
	tests := []struct {
		name string
		user string
		want []string
	}{
		{
			name: "empty default",
			want: []string{"docker", "exec", "--user", "1000:1000", "--env", "HOME=/config", "--env", "USER=abc", "box-bx_abcdefghijkl", "id"},
		},
		{
			name: "root",
			user: "root",
			want: []string{"docker", "exec", "--user", "root", "box-bx_abcdefghijkl", "id"},
		},
		{
			name: "other user",
			user: "worker",
			want: []string{"docker", "exec", "--user", "worker", "box-bx_abcdefghijkl", "id"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &scriptedExecutor{}
			if _, err := New(exec).Exec(context.Background(), "box-bx_abcdefghijkl", tt.user, "", nil, []string{"id"}, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(exec.calls[0].argv, tt.want) {
				t.Fatalf("Exec() argv = %#v, want %#v", exec.calls[0].argv, tt.want)
			}
		})
	}
}

func TestDiffParsesKindsAndPaths(t *testing.T) {
	exec := &scriptedExecutor{responses: []struct {
		code   int
		err    error
		stdout string
		stderr string
	}{{stdout: "A /new file\nC /etc/issue\nD /deleted\n"}}}
	changes, err := New(exec).Diff(context.Background(), "box-bx_abcdefghijkl")
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{{Kind: "A", Path: "/new file"}, {Kind: "C", Path: "/etc/issue"}, {Kind: "D", Path: "/deleted"}}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("Diff() = %#v, want %#v", changes, want)
	}
}

func TestCopyInStreamsTarAndUsesArchiveCopy(t *testing.T) {
	exec := &scriptedExecutor{}
	if err := New(exec).CopyIn(context.Background(), "box-bx_abcdefghijkl", "/", strings.NewReader("tar archive")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(exec.calls[0].argv, []string{"docker", "cp", "-a", "-", "box-bx_abcdefghijkl:/"}) || exec.calls[0].stdin != "tar archive" {
		t.Fatalf("CopyIn call = %#v", exec.calls[0])
	}
}

func TestCommandErrorRedactsCredentialValues(t *testing.T) {
	exec := &scriptedExecutor{responses: []struct {
		code   int
		err    error
		stdout string
		stderr string
	}{{code: 1, stderr: "failed PASSWORD=desktop-secret BOXD_TOKEN=agent-secret"}}}
	_, err := New(exec).Create(context.Background(), Spec{BoxID: "bx_abcdefghijkl", Image: "image", Type: "small", Runtime: "runc", DesktopPassword: "desktop-secret", BoxdToken: "agent-secret"})
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		t.Fatalf("Create() error = %v, want *CommandError", err)
	}
	if strings.Contains(err.Error(), "desktop-secret") || strings.Contains(err.Error(), "agent-secret") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error did not redact credentials: %v", err)
	}
}

func TestCreateErrorRedactsEnvironmentSecrets(t *testing.T) {
	exec := &scriptedExecutor{responses: []struct {
		code   int
		err    error
		stdout string
		stderr string
	}{{code: 1, stderr: "failed API_KEY=environment-secret"}}}
	_, err := New(exec).Create(context.Background(), Spec{
		BoxID: "bx_abcdefghijkl", Image: "image", Type: "small", Runtime: "runc",
		Env: map[string]string{"API_KEY": "environment-secret"},
	})
	if err == nil || strings.Contains(err.Error(), "environment-secret") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("Create() error did not redact environment secret: %v", err)
	}
}

func TestCreateArgsPointsBoxAtDNSServerOnlyWhenSet(t *testing.T) {
	base := Spec{BoxID: "bx_abcdefghijkl", Image: "boxes/box:dev", Type: "small", Runtime: "sysbox-runc"}
	without, err := CreateArgs(base)
	if err != nil {
		t.Fatal(err)
	}
	if containsArg(without, "--dns") {
		t.Fatalf("CreateArgs without DNSServer has --dns: %q", without)
	}
	base.DNSServer = "172.18.0.1"
	with, err := CreateArgs(base)
	if err != nil {
		t.Fatal(err)
	}
	if !containsPair(with, "--dns", "172.18.0.1") {
		t.Fatalf("CreateArgs with DNSServer = %q, want --dns 172.18.0.1", with)
	}
	base.DNSServer = "not-an-ip"
	if _, err := CreateArgs(base); err == nil {
		t.Fatal("CreateArgs accepted a non-IP DNS server")
	}
}

func containsArg(argv []string, arg string) bool {
	for _, value := range argv {
		if value == arg {
			return true
		}
	}
	return false
}

func containsPair(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}
