package snapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type executorCall struct {
	argv  []string
	stdin string
}

type executorReply struct {
	stdout string
	stderr string
	code   int
	err    error
}

type fakeExecutor struct {
	replies []executorReply
	calls   []executorCall
	backup  string
	pid     string
	envs    []map[string]string
}

func (f *fakeExecutor) Run(_ context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	call := executorCall{argv: append([]string(nil), argv...)}
	if stdin != nil {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return 0, err
		}
		call.stdin = string(data)
	}
	f.calls = append(f.calls, call)
	if reflect.DeepEqual(argv, []string{"docker", "inspect", "-f", "{{.State.Pid}}", "box-bx_abcdefghijkl"}) {
		pid := f.pid
		if pid == "" {
			pid = "999999"
		}
		if stdout != nil {
			_, _ = io.WriteString(stdout, pid+"\n")
		}
		return 0, nil
	}

	if len(argv) > 0 && argv[0] == "restic" && contains(argv, "backup") {
		f.backup = call.stdin
	}
	var reply executorReply
	if len(f.replies) > 0 {
		reply = f.replies[0]
		f.replies = f.replies[1:]
	}
	if stdout != nil {
		_, _ = io.WriteString(stdout, reply.stdout)
	}
	if stderr != nil {
		_, _ = io.WriteString(stderr, reply.stderr)
	}
	return reply.code, reply.err
}

func (f *fakeExecutor) RunWithEnv(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer, env map[string]string) (int, error) {
	copy := make(map[string]string, len(env))
	for key, value := range env {
		copy[key] = value
	}
	f.envs = append(f.envs, copy)
	return f.Run(ctx, argv, stdin, stdout, stderr)
}

func TestResticInvocationReceivesS3CredentialsAsEnvironment(t *testing.T) {
	restic := &fakeExecutor{replies: []executorReply{{}}}
	engine := New(&fakeExecutor{}, restic, "s3:https://s3.example.test/boxes", "restic-password")
	engine.Env = map[string]string{"AWS_ACCESS_KEY_ID": "access-value", "AWS_SECRET_ACCESS_KEY": "secret-value", "AWS_DEFAULT_REGION": "us-east-1"}
	if err := engine.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(restic.envs) != 1 || restic.envs[0]["AWS_ACCESS_KEY_ID"] != "access-value" || restic.envs[0]["AWS_SECRET_ACCESS_KEY"] != "secret-value" || restic.envs[0]["AWS_DEFAULT_REGION"] != "us-east-1" {
		t.Fatalf("restic environment = %#v, expected S3 credentials", restic.envs)
	}
}

func TestSnapshotVolumesUseOnlyImageDeclaredVolumes(t *testing.T) {
	docker := &fakeExecutor{replies: []executorReply{{stdout: `{"/config":{}}`}}}
	engine := New(docker, &fakeExecutor{}, "/repo", "pw")
	volumes, err := engine.volumePaths(context.Background(), "box-bx_abcdefghijkl")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(volumes, []string{"/config"}) {
		t.Fatalf("snapshot volumes = %#v, want only image-declared /config; runtime tools mount must be excluded", volumes)
	}
}

func TestDefaultExcludesBrowserAndToolCachesButKeepsProfiles(t *testing.T) {
	for _, excluded := range []string{
		"/config/.cache/tool/index", "/config/.config/google-chrome/Default/Cache/data",
		"/config/.config/chromium/Profile 1/Service Worker/CacheStorage/x",
		"/config/.mozilla/firefox/abc/cache2/x", "/config/.npm/_cacache/x",
		"/config/go/pkg/mod/cache/download/x", "/config/.cargo/registry/cache/x",
	} {
		if !isExcluded(excluded) {
			t.Errorf("%q should be excluded", excluded)
		}
	}
	for _, included := range []string{"/config/.config/google-chrome/Default/Cookies", "/config/.mozilla/firefox/abc/cookies.sqlite"} {
		if isExcluded(included) {
			t.Errorf("%q should be captured", included)
		}
	}
}

type fixedMountInfo string

func (f fixedMountInfo) ReadMountInfo(string) (string, error) { return string(f), nil }

func TestFingerprintStableAndChangesWithMetadata(t *testing.T) {
	upper := t.TempDir()
	file := filepath.Join(upper, "changed.txt")
	if err := os.WriteFile(file, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	mount := "1 0 0:1 / / rw - overlay overlay rw,upperdir=" + upper + "\n"
	docker := &fakeExecutor{replies: []executorReply{{stdout: "{}\n"}, {stdout: "{}\n"}, {stdout: "{}\n"}}}
	engine := New(docker, &fakeExecutor{}, "/repo", "pw")
	engine.mountInfo = fixedMountInfo(mount)
	f1, err := engine.Fingerprint(context.Background(), "bx_abcdefghijkl")
	if err != nil {
		t.Fatal(err)
	}
	f2, err := engine.Fingerprint(context.Background(), "bx_abcdefghijkl")
	if err != nil {
		t.Fatal(err)
	}
	if f1 == "" || f1 != f2 {
		t.Fatalf("fingerprints = %q, %q; want stable nonempty", f1, f2)
	}
	if err := os.WriteFile(file, []byte("two longer"), 0600); err != nil {
		t.Fatal(err)
	}
	f3, err := engine.Fingerprint(context.Background(), "bx_abcdefghijkl")
	if err != nil {
		t.Fatal(err)
	}
	if f3 == f2 {
		t.Fatal("fingerprint did not change after file metadata changed")
	}
}

func TestCaptureFiltersChangesAndCreatesResticSnapshot(t *testing.T) {
	docker := &fakeExecutor{replies: []executorReply{
		{stdout: "A /etc/issue\nC /config/keep.txt\nA /config/ignored.log\nD /config/gone.txt\nD /.boxes/manifest\nD /.boxes/opaque\nA /proc/1/status\nC /config/.boxignore\nC /.boxes/deletions\n"},
		{stdout: `{"/config":{}}`},       // inspect image-declared volumes
		{stdout: "/config/.boxignore\n"}, // find .boxignore files
		{stdout: "*.log\n"},              // read .boxignore
		{},                               // volume enumeration (empty)
		{},                               // create /.boxes
		{},                               // write /.boxes/deletions
		{},                               // write /.boxes/manifest
		{},                               // write /.boxes/opaque
		{stdout: "/.boxes/deletions\x00/.boxes/manifest\x00/.boxes/opaque\x00/config/.boxignore\x00/config/keep.txt\x00/etc/issue\x00"}, // filter fallback paths
		{stdout: "tar-data"}, // docker tar
	}}
	restic := &fakeExecutor{replies: []executorReply{{stdout: "{\"message_type\":\"summary\",\"snapshot_id\":\"snap-123\",\"total_bytes_processed\":42}\n"}}}
	engine := New(docker, restic, "/repo", "restic-secret")

	snapshot, err := engine.Capture(context.Background(), "bx_abcdefghijkl")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(snapshot.ID, "sn_") || len(snapshot.ID) != len("sn_")+12 || snapshot.ResticID != "snap-123" || snapshot.BoxID != "bx_abcdefghijkl" || snapshot.SizeBytes != 42 || snapshot.CreatedAt.IsZero() {
		t.Fatalf("Capture() = %#v", snapshot)
	}
	if len(docker.calls) != 12 || len(restic.calls) != 1 {
		t.Fatalf("Capture() made docker calls %#v and restic calls %#v", docker.calls, restic.calls)
	}
	if docker.calls[7].stdin != "/config/gone.txt\n" {
		t.Fatalf("deletion list = %q", docker.calls[7].stdin)
	}
	if docker.calls[8].stdin != "/config/.boxignore\x00/config/keep.txt\x00/etc/issue\x00" {
		t.Fatalf("manifest path list = %q", docker.calls[8].stdin)
	}
	if docker.calls[11].stdin != "/.boxes/deletions\x00/.boxes/manifest\x00/.boxes/opaque\x00/config/.boxignore\x00/config/keep.txt\x00/etc/issue\x00" {
		t.Fatalf("tar path list = %q", docker.calls[11].stdin)
	}
	if !contains(docker.calls[11].argv, "--null") {
		t.Fatalf("docker tar argv does not enable NUL-delimited paths: %#v", docker.calls[11].argv)
	}
	if restic.backup != "tar-data" {
		t.Fatalf("restic backup stdin = %q, want docker tar output", restic.backup)
	}
	backupArgs := restic.calls[0].argv
	if !reflect.DeepEqual(backupArgs[:3], []string{"restic", "-r", "/repo"}) || !contains(backupArgs, "--stdin") || !contains(backupArgs, "--stdin-filename") || !contains(backupArgs, "rootfs.tar") || !contains(backupArgs, "--json") || !contains(backupArgs, "box=bx_abcdefghijkl") {
		t.Fatalf("restic backup argv = %#v", backupArgs)
	}
	if contains(backupArgs, "restic-secret") {
		t.Fatal("restic password appeared in command arguments")
	}
}

func TestCaptureIncludesTarStderrWhenTarExitsNonzero(t *testing.T) {
	const tarError = "tar: /etc/ssh/ssh_host_ed25519_key: file changed as we read it"
	docker := &fakeExecutor{replies: []executorReply{
		{stdout: "A /etc/issue\n"},
		{stdout: "null"},
		{},
		{},
		{},
		{},
		{},
		{stdout: "/.boxes/deletions\x00/.boxes/manifest\x00/.boxes/opaque\x00/etc/issue\x00"},
		{stderr: tarError, code: 2},
	}}
	restic := &fakeExecutor{replies: []executorReply{{stdout: `{"snapshot_id":"snapshot-123","total_bytes_processed":1}`}}}
	_, err := New(docker, restic, "/repo", "restic-secret").Capture(context.Background(), "bx_abcdefghijkl")
	if err == nil || !strings.Contains(err.Error(), tarError) {
		t.Fatalf("Capture() error = %v, want tar stderr %q", err, tarError)
	}
}

func TestSnapshotPathFilterDropsSocketsAndMissingPathsButKeepsSymlinks(t *testing.T) {
	directory := t.TempDir()
	regular := filepath.Join(directory, "regular file")
	link := filepath.Join(directory, "link")
	socketFile, err := os.CreateTemp("/tmp", "bss-")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	if err := socketFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(directory, "vanished")
	if err := os.WriteFile(regular, []byte("captured"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	socket, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = socket.Close()
		_ = os.Remove(socketPath)
	}()

	input := strings.Join([]string{regular, link, socketPath, missing}, "\x00") + "\x00"
	command := exec.Command("bash", "-c", snapshotPathFilterScript)
	command.Stdin = strings.NewReader(input)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("run snapshot path filter: %v", err)
	}
	if got, want := string(output), regular+"\x00"+link+"\x00"; got != want {
		t.Fatalf("filtered paths = %q, want %q", got, want)
	}
}

func TestCaptureIncludesDeclaredVolumeContentsAndFiltersThem(t *testing.T) {
	docker := &fakeExecutor{replies: []executorReply{
		{stdout: "A /etc/issue\n"},
		{stdout: `{"/config":{},"/data":{}}`},
		{stdout: "/config/.boxignore\n"},
		{stdout: "*.ignored\n"},
		{stdout: "/config\x00/config/home.txt\x00/config/.cache\x00/config/.cache/token\x00/config/.boxignore\x00/config/hidden.ignored\x00"},
		{stdout: "/data\x00/data/state.txt\x00"},
		{}, // create /.boxes
		{}, // write /.boxes/deletions
		{}, // write /.boxes/manifest
		{}, // write /.boxes/opaque
		{stdout: "/.boxes/deletions\x00/.boxes/manifest\x00/.boxes/opaque\x00/config\x00/config/.boxignore\x00/config/home.txt\x00/data\x00/data/state.txt\x00/etc/issue\x00"}, // filter fallback paths
		{stdout: "tar-data"},
	}}
	restic := &fakeExecutor{replies: []executorReply{{stdout: `{"snapshot_id":"snapshot-volume","total_bytes_processed":12}`}}}

	_, err := New(docker, restic, "/repo", "secret").Capture(context.Background(), "bx_abcdefghijkl")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(docker.calls[2].argv, []string{"docker", "inspect", "--format", "{{json .Config.Volumes}}", "box-bx_abcdefghijkl"}) {
		t.Fatalf("volume inspect argv = %#v", docker.calls[2].argv)
	}
	if !reflect.DeepEqual(docker.calls[5].argv, []string{"docker", "exec", "--user", "0:0", "box-bx_abcdefghijkl", "find", "/config", "-xdev", "-print0"}) ||
		!reflect.DeepEqual(docker.calls[6].argv, []string{"docker", "exec", "--user", "0:0", "box-bx_abcdefghijkl", "find", "/data", "-xdev", "-print0"}) {
		t.Fatalf("volume enumeration calls = %#v and %#v", docker.calls[5].argv, docker.calls[6].argv)
	}
	const wantPaths = "/.boxes/deletions\x00/.boxes/manifest\x00/.boxes/opaque\x00/config\x00/config/.boxignore\x00/config/home.txt\x00/data\x00/data/state.txt\x00/etc/issue\x00"
	if docker.calls[12].stdin != wantPaths {
		t.Fatalf("tar path list = %q, want %q", docker.calls[12].stdin, wantPaths)
	}
}

func TestCaptureRunsEveryDockerExecAsRootWithoutAgentEnvironment(t *testing.T) {
	docker := &fakeExecutor{replies: []executorReply{
		{stdout: "A /etc/issue\n"},
		{stdout: `{"/config":{},"/data":{}}`},
		{stdout: "/config/.boxignore\n"},
		{stdout: "*.ignored\n"},
		{stdout: "/config\x00"},
		{stdout: "/data\x00"},
		{},
		{},
		{},
		{},
		{stdout: "/.boxes/deletions\x00/.boxes/manifest\x00/.boxes/opaque\x00/config\x00/data\x00/etc/issue\x00"},
		{stdout: "tar-data"},
	}}
	restic := &fakeExecutor{replies: []executorReply{{stdout: `{"snapshot_id":"snapshot-root","total_bytes_processed":1}`}}}
	if _, err := New(docker, restic, "/repo", "secret").Capture(context.Background(), "bx_abcdefghijkl"); err != nil {
		t.Fatal(err)
	}

	execCalls := 0
	for _, call := range docker.calls {
		argv := call.argv
		if len(argv) < 2 || argv[0] != "docker" || argv[1] != "exec" {
			continue
		}
		execCalls++
		user := ""
		for i, arg := range argv[:len(argv)-1] {
			if arg == "--user" {
				user = argv[i+1]
				break
			}
		}
		if user != "0:0" {
			t.Errorf("snapshot docker exec argv = %#v, want --user 0:0", argv)
		}
		if contains(argv, "1000:1000") {
			t.Errorf("snapshot docker exec ran as agent user 1000:1000: %#v", argv)
		}
		if contains(argv, "HOME=/config") || contains(argv, "USER=abc") {
			t.Errorf("snapshot docker exec included agent environment: %#v", argv)
		}
	}
	if execCalls != 10 {
		t.Fatalf("Capture() made %d docker exec calls, want 10", execCalls)
	}
}

func TestRestoreStreamsResticArchiveIntoDockerCopy(t *testing.T) {
	docker := &fakeExecutor{}
	restic := &fakeExecutor{replies: []executorReply{{stdout: "archive bytes"}}}
	engine := New(docker, restic, "/repo", "secret")
	if err := engine.Restore(context.Background(), "bx_abcdefghijkl", "snap-123"); err != nil {
		t.Fatal(err)
	}
	if len(restic.calls) != 1 || !reflect.DeepEqual(restic.calls[0].argv[:3], []string{"restic", "-r", "/repo"}) || !contains(restic.calls[0].argv, "dump") || !contains(restic.calls[0].argv, "rootfs.tar") {
		t.Fatalf("restic dump call = %#v", restic.calls)
	}
	if !reflect.DeepEqual(docker.calls[0].argv, []string{"docker", "cp", "-a", "-", "box-bx_abcdefghijkl:/"}) || docker.calls[0].stdin != "archive bytes" {
		t.Fatalf("docker copy call = %#v", docker.calls[0])
	}
}

func TestFileReturnsDirectoryAsTarArchive(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, item := range []struct {
		name, body string
		kind       byte
	}{
		{"config/", "", tar.TypeDir}, {"config/app.txt", "app-data", tar.TypeReg}, {"config/nested/", "", tar.TypeDir}, {"config/nested/deep.txt", "deep-data", tar.TypeReg}, {"elsewhere.txt", "other", tar.TypeReg},
	} {
		header := &tar.Header{Name: item.name, Mode: 0o644, Typeflag: item.kind, Size: int64(len(item.body))}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if item.body != "" {
			if _, err := io.WriteString(writer, item.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	engine := New(&fakeExecutor{}, &fakeExecutor{replies: []executorReply{{stdout: archive.String()}}}, "/repo", "secret")
	result, err := engine.File(context.Background(), "restic-snapshot", "/config")
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(result))
	var names []string
	contents := map[string]string{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		contents[header.Name] = string(data)
	}
	if !reflect.DeepEqual(names, []string{".", "app.txt", "nested", "nested/deep.txt"}) {
		t.Fatalf("tar entries = %#v", names)
	}
	if contents["app.txt"] != "app-data" || contents["nested/deep.txt"] != "deep-data" {
		t.Fatalf("tar contents = %#v", contents)
	}
}

func TestRestoreCanUseSnapshotSpecificRepositoryAndCredentials(t *testing.T) {
	docker := &fakeExecutor{}
	restic := &fakeExecutor{replies: []executorReply{{stdout: "old archive"}}}
	engine := New(docker, restic, "/current/repository", "current-password")
	err := engine.RestoreWithStorage(context.Background(), "bx_abcdefghijkl", "old-snapshot", "s3:https://old.example.test/boxes", map[string]string{
		"AWS_ACCESS_KEY_ID": "old-access", "AWS_SECRET_ACCESS_KEY": "old-secret", "RESTIC_PASSWORD": "old-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restic.calls[0].argv[:3], []string{"restic", "-r", "s3:https://old.example.test/boxes"}) {
		t.Fatalf("old snapshot restore repository = %#v", restic.calls[0].argv)
	}
	if restic.envs[0]["AWS_ACCESS_KEY_ID"] != "old-access" || restic.envs[0]["AWS_SECRET_ACCESS_KEY"] != "old-secret" {
		t.Fatalf("old snapshot restore environment = %#v", restic.envs)
	}
}

func TestTreeAndFileReadTarContentsFromResticDump(t *testing.T) {
	archive := tarBytes(t, []struct {
		name string
		body string
		mode byte
	}{
		{name: "config/", mode: tar.TypeDir},
		{name: "config/hello.txt", body: "hello", mode: tar.TypeReg},
		{name: "config/link", body: "target", mode: tar.TypeSymlink},
	})
	treeExecutor := &fakeExecutor{replies: []executorReply{{stdout: string(archive)}}}
	entries, err := New(&fakeExecutor{}, treeExecutor, "/repo", "secret").Tree(context.Background(), "snap-123")
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{Path: "/config", Size: 0, Type: "dir"},
		{Path: "/config/hello.txt", Size: 5, Type: "file"},
		{Path: "/config/link", Size: 0, Type: "symlink"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("Tree() = %#v, want %#v", entries, want)
	}

	fileExecutor := &fakeExecutor{replies: []executorReply{{stdout: string(archive)}}}
	data, err := New(&fakeExecutor{}, fileExecutor, "/repo", "secret").File(context.Background(), "snap-123", "/config/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("File() = %q, want hello", data)
	}
}

func TestForgetAndInitUseResticRepository(t *testing.T) {
	forgetExecutor := &fakeExecutor{}
	if err := New(&fakeExecutor{}, forgetExecutor, "/repo", "secret").Forget(context.Background(), "snap-123"); err != nil {
		t.Fatal(err)
	}
	if !contains(forgetExecutor.calls[0].argv, "forget") || !contains(forgetExecutor.calls[0].argv, "--prune") || !contains(forgetExecutor.calls[0].argv, "snap-123") {
		t.Fatalf("forget argv = %#v", forgetExecutor.calls[0].argv)
	}

	initExecutor := &fakeExecutor{replies: []executorReply{{code: 1, stderr: "repository does not exist"}, {}}}
	if err := New(&fakeExecutor{}, initExecutor, "/repo", "secret").Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(initExecutor.calls) != 2 || !contains(initExecutor.calls[0].argv, "cat") || !contains(initExecutor.calls[0].argv, "config") || !contains(initExecutor.calls[1].argv, "init") {
		t.Fatalf("init calls = %#v", initExecutor.calls)
	}
}

func TestForgetManyAndPruneAreSeparateCommands(t *testing.T) {
	executor := &fakeExecutor{}
	engine := New(&fakeExecutor{}, executor, "/repo", "secret")
	ids := []string{"aabb", "0011"}
	if err := engine.ForgetMany(context.Background(), ids); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 1 || !contains(executor.calls[0].argv, "forget") || contains(executor.calls[0].argv, "--prune") || !contains(executor.calls[0].argv, "aabb") || !contains(executor.calls[0].argv, "0011") {
		t.Fatalf("ForgetMany calls = %#v", executor.calls)
	}
	if err := engine.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 2 || !contains(executor.calls[1].argv, "prune") {
		t.Fatalf("Prune calls = %#v", executor.calls)
	}
}

func TestResticStaleLocksAreUnlockedAndWriteRetriedOnce(t *testing.T) {
	for _, command := range []string{"forget", "backup"} {
		t.Run(command, func(t *testing.T) {
			executor := &fakeExecutor{replies: []executorReply{
				{code: 1, stderr: "unable to create lock in backend: repository is already locked"},
				{}, {},
			}}
			engine := New(&fakeExecutor{}, executor, "/repo", "secret")
			var err error
			if command == "forget" {
				err = engine.Forget(context.Background(), "snap-123")
			} else {
				err = engine.runRestic(context.Background(), nil, nil, "backup", "/data")
			}
			if err != nil {
				t.Fatalf("%s after stale lock recovery: %v", command, err)
			}
			if len(executor.calls) != 3 || !contains(executor.calls[0].argv, command) || !contains(executor.calls[1].argv, "unlock") || !contains(executor.calls[2].argv, command) {
				t.Fatalf("restic calls = %#v, want command, unlock, command", executor.calls)
			}
			for _, call := range executor.calls {
				if contains(call.argv, "--remove-all") {
					t.Fatalf("restic call used unsafe --remove-all: %#v", call.argv)
				}
			}
		})
	}
}

func TestResticCommandsUseSafeRepositoryLockingFlags(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantFlag   string
		wantValue  string
		forbidFlag string
	}{
		{name: "backup", args: []string{"backup"}, wantFlag: "--retry-lock", wantValue: "5m", forbidFlag: "--no-lock"},
		{name: "forget", args: []string{"forget", "--prune", "snap-123"}, wantFlag: "--retry-lock", wantValue: "5m", forbidFlag: "--no-lock"},
		{name: "init", args: []string{"init"}, wantFlag: "--retry-lock", wantValue: "5m", forbidFlag: "--no-lock"},
		{name: "prune", args: []string{"prune"}, wantFlag: "--retry-lock", wantValue: "5m", forbidFlag: "--no-lock"},
		{name: "unlock", args: []string{"unlock"}, wantFlag: "--retry-lock", wantValue: "5m", forbidFlag: "--no-lock"},
		{name: "check", args: []string{"check"}, wantFlag: "--retry-lock", wantValue: "5m", forbidFlag: "--no-lock"},
		{name: "dump", args: []string{"dump", "snap-123"}, wantFlag: "--no-lock", forbidFlag: "--retry-lock"},
		{name: "ls", args: []string{"ls", "snap-123"}, wantFlag: "--no-lock", forbidFlag: "--retry-lock"},
		{name: "snapshots", args: []string{"snapshots"}, wantFlag: "--no-lock", forbidFlag: "--retry-lock"},
		{name: "cat", args: []string{"cat", "config"}, wantFlag: "--no-lock", forbidFlag: "--retry-lock"},
		{name: "stats", args: []string{"stats"}, wantFlag: "--no-lock", forbidFlag: "--retry-lock"},
		{name: "find", args: []string{"find", "/"}, wantFlag: "--no-lock", forbidFlag: "--retry-lock"},
		{name: "diff", args: []string{"diff", "snap-123", "snap-456"}, wantFlag: "--no-lock", forbidFlag: "--retry-lock"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &fakeExecutor{}
			engine := New(&fakeExecutor{}, executor, "/repo", "secret")
			if err := engine.runRestic(context.Background(), nil, nil, test.args...); err != nil {
				t.Fatalf("run restic %s: %v", test.args[0], err)
			}
			argv := executor.calls[0].argv
			if !contains(argv, test.wantFlag) {
				t.Errorf("restic argv = %#v, missing %s", argv, test.wantFlag)
			}
			if test.wantValue != "" && !contains(argv, test.wantValue) {
				t.Errorf("restic argv = %#v, missing value %q for %s", argv, test.wantValue, test.wantFlag)
			}
			if contains(argv, test.forbidFlag) {
				t.Errorf("restic argv = %#v, must not contain %s", argv, test.forbidFlag)
			}
		})
	}
}

func TestResticCommandsSetCacheDirWhenHomeIsUnset(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	executor := &fakeExecutor{}
	if err := New(&fakeExecutor{}, executor, "/repo", "secret").runRestic(context.Background(), nil, nil, "forget"); err != nil {
		t.Fatalf("run restic forget: %v", err)
	}
	argv := executor.calls[0].argv
	for index, argument := range argv {
		if argument == "--cache-dir" {
			if index+1 >= len(argv) || argv[index+1] == "" {
				t.Fatalf("restic argv has an empty --cache-dir: %#v", argv)
			}
			return
		}
	}
	t.Fatalf("restic argv = %#v, missing explicit --cache-dir", argv)
}

func TestDestructiveForgetKeepsRequiredRepositoryLock(t *testing.T) {
	executor := &fakeExecutor{}
	engine := New(&fakeExecutor{}, executor, "/repo", "secret")
	if err := engine.Forget(context.Background(), "snap-123"); err != nil {
		t.Fatalf("forget snapshot: %v", err)
	}
	if contains(executor.calls[0].argv, "--no-lock") {
		t.Fatalf("destructive forget argv = %#v; installed restic requires repository locking for forget --prune", executor.calls[0].argv)
	}
}

func TestCaptureReturnsErrorWhenResticBackupFails(t *testing.T) {
	docker := &fakeExecutor{replies: []executorReply{{}, {stdout: "{}"}, {}, {}, {}, {}, {}, {stdout: "tar"}}}
	restic := &fakeExecutor{replies: []executorReply{{code: 1, stderr: "backup failed"}}}
	_, err := New(docker, restic, "/repo", "secret").Capture(context.Background(), "bx_abcdefghijkl")
	if err == nil || !strings.Contains(err.Error(), "backup") {
		t.Fatalf("Capture() error = %v, want backup failure", err)
	}
}

func TestCaptureUsesEmptyDeletionListWithoutAddingDeletedPathsToTar(t *testing.T) {
	docker := &fakeExecutor{replies: []executorReply{
		{stdout: "A /config/only.txt\n"}, {stdout: "{}"}, {stdout: ""}, {}, {}, {}, {}, {stdout: "tar"},
	}}
	restic := &fakeExecutor{replies: []executorReply{{stdout: "{\"snapshot_id\":\"snap-empty-deletions\",\"total_bytes_processed\":1}\n"}}}
	_, err := New(docker, restic, "/repo", "secret").Capture(context.Background(), "bx_abcdefghijkl")
	if err != nil {
		t.Fatal(err)
	}
	if docker.calls[5].stdin != "" {
		t.Fatalf("empty deletion list = %q", docker.calls[5].stdin)
	}
	if docker.calls[8].stdin != "/.boxes/deletions\x00/.boxes/manifest\x00/.boxes/opaque\x00/config/only.txt\x00" {
		t.Fatalf("tar path list = %q", docker.calls[8].stdin)
	}
}

func TestSnapshotExcludesContractPathsAndTheirContents(t *testing.T) {
	for _, filePath := range []string{
		"/proc/1/status", "/sys/kernel", "/dev/null", "/run/lock", "/tmp/file",
		"/var/tmp/file", "/var/lib/docker/overlay", "/var/cache/apt/archives/pkg",
		"/config/.cache/tool/cache", "/.boxes/restored/marker", "/opt/boxes-tools/bin/codex",
	} {
		if !isExcluded(filePath) {
			t.Errorf("isExcluded(%q) = false", filePath)
		}
	}
	for _, filePath := range []string{"/tmpish/file", "/config/.cache-old/file", "/.boxes/restored-old/file", "/opt/boxes-tools-extra/bin/codex"} {
		if isExcluded(filePath) {
			t.Errorf("isExcluded(%q) = true", filePath)
		}
	}
}

func TestInitDoesNotReinitializeExistingRepository(t *testing.T) {
	executor := &fakeExecutor{}
	if err := New(&fakeExecutor{}, executor, "/repo", "secret").Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 1 || !contains(executor.calls[0].argv, "cat") {
		t.Fatalf("Init() calls = %#v, want only cat config", executor.calls)
	}
}

func TestInitTreatsConcurrentInitializationAsSuccessWhenConfigExists(t *testing.T) {
	executor := &fakeExecutor{replies: []executorReply{
		{code: 1, stderr: "repository does not exist"},
		{code: 1, stderr: "Fatal: config file already exists"},
		{stdout: "repository config"},
	}}
	if err := New(&fakeExecutor{}, executor, "/repo", "secret").Init(context.Background()); err != nil {
		t.Fatalf("Init() after concurrent initialization: %v", err)
	}
	if len(executor.calls) != 3 || !contains(executor.calls[1].argv, "init") || !contains(executor.calls[2].argv, "cat") || !contains(executor.calls[2].argv, "config") {
		t.Fatalf("Init() calls = %#v, want cat config, init, and verification cat config", executor.calls)
	}
}

func TestResticErrorsDoNotExposeRepositoryOrPassword(t *testing.T) {
	secretRepo := "s3:https://user:repo-secret@example.test/bucket"
	secretPassword := "restic-secret"
	executor := &fakeExecutor{replies: []executorReply{{code: 1, stderr: "failed at " + secretRepo + " with " + secretPassword}}}
	err := New(&fakeExecutor{}, executor, secretRepo, secretPassword).Forget(context.Background(), "snap-123")
	if err == nil || strings.Contains(err.Error(), secretRepo) || strings.Contains(err.Error(), secretPassword) {
		t.Fatalf("Restic error leaked credentials: %v", err)
	}
}

func TestResticErrorsDoNotExposeS3Credentials(t *testing.T) {
	accessKey := "s3-access-secret-value"
	secretKey := "s3-secret-secret-value"
	executor := &fakeExecutor{replies: []executorReply{{code: 1, stderr: "request failed with " + accessKey + " and " + secretKey}}}
	engine := New(&fakeExecutor{}, executor, "s3:https://example.test/bucket", "restic-secret")
	engine.Env = map[string]string{"AWS_ACCESS_KEY_ID": accessKey, "AWS_SECRET_ACCESS_KEY": secretKey}
	err := engine.Forget(context.Background(), "snap-123")
	if err == nil || strings.Contains(err.Error(), accessKey) || strings.Contains(err.Error(), secretKey) {
		t.Fatalf("Restic error leaked S3 credentials: %v", err)
	}
}

func TestFileRejectsMissingPath(t *testing.T) {
	archive := tarBytes(t, []struct {
		name string
		body string
		mode byte
	}{{name: "config/hello.txt", body: "hello", mode: tar.TypeReg}})
	executor := &fakeExecutor{replies: []executorReply{{stdout: string(archive)}}}
	_, err := New(&fakeExecutor{}, executor, "/repo", "secret").File(context.Background(), "snap-123", "/missing")
	if err == nil {
		t.Fatal("File() succeeded for a missing path")
	}
}

func tarBytes(t *testing.T, files []struct {
	name string
	body string
	mode byte
}) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	for _, file := range files {
		header := &tar.Header{Name: file.name, Mode: 0o644, Size: int64(len(file.body)), Typeflag: file.mode, ModTime: time.Unix(1, 0)}
		if file.mode == tar.TypeDir {
			header.Size = 0
		}
		if file.mode == tar.TypeSymlink {
			header.Linkname = file.body
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if file.mode == tar.TypeReg {
			if _, err := io.WriteString(writer, file.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func contains(argv []string, value string) bool {
	for _, arg := range argv {
		if arg == value {
			return true
		}
	}
	return false
}

func TestResticBackupLockErrorClearsStaleLocksWithoutReplay(t *testing.T) {
	executor := &fakeExecutor{replies: []executorReply{
		{code: 1, stderr: "unable to create lock in backend: repository is already locked exclusively"},
		{},
	}}
	engine := New(&fakeExecutor{}, executor, "/repo", "secret")
	if err := engine.runRestic(context.Background(), strings.NewReader("tar"), nil, "backup", "--stdin"); err == nil {
		t.Fatal("backup lock error was swallowed; want it returned so the next capture retries")
	}
	if len(executor.calls) != 2 || !contains(executor.calls[1].argv, "unlock") || contains(executor.calls[1].argv, "--remove-all") {
		t.Fatalf("restic calls = %#v, want backup then plain unlock", executor.calls)
	}
}
