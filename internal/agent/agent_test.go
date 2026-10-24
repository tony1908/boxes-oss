package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tony1908/boxes-oss/internal/dockercli"
	"github.com/tony1908/boxes-oss/internal/snapshot"
	"gopkg.in/yaml.v3"
)

type callRecord struct {
	argv  []string
	stdin []byte
}

type testExecutor struct {
	name    string
	calls   *[]callRecord
	command func([]string, []byte, io.Writer, io.Writer) (int, error)
}

func (e testExecutor) Run(_ context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	var input []byte
	if stdin != nil {
		var err error
		input, err = io.ReadAll(stdin)
		if err != nil {
			return 1, err
		}
	}
	if e.calls != nil {
		*e.calls = append(*e.calls, callRecord{argv: append([]string(nil), argv...), stdin: input})
	}
	if e.command != nil {
		return e.command(argv, input, stdout, stderr)
	}
	return 0, nil
}

func TestBoxdProxyUsesSingleLeadingSlashForUpstreamPath(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:7070")
	if err != nil {
		t.Skipf("boxd proxy test port is unavailable: %v", err)
	}
	defer listener.Close()
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("upstream path = %q, want /healthz", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer boxd-test-token" {
			t.Errorf("upstream authorization = %q, want boxd token", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	upstream.Listener = listener
	upstream.Start()
	defer upstream.Close()

	const boxID = "bx_boxdproxyabc"
	docker := dockercli.New(testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) >= 3 && argv[1] == "inspect" && argv[2] == "box-"+boxID {
			_, _ = io.WriteString(stdout, `[{"Id":"container","Name":"/box-`+boxID+`","State":{"Status":"running"},"NetworkSettings":{"Networks":{"boxes-net":{"IPAddress":"127.0.0.1"}}}}]`)
			return 0, nil
		}
		return 1, errors.New("unexpected Docker command")
	}})
	service := New(Config{NodeID: "nd_test", Runtime: "runc"}, docker, snapshot.New(testExecutor{}, testExecutor{}, "/repo", "password"))
	service.boxdTokens[boxID] = "boxd-test-token"
	service.desktopPasswords[boxID] = "desktop-test-password"
	request := httptest.NewRequest(http.MethodGet, "/v1/boxes/"+boxID+"/boxd/healthz", nil)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != `{"status":"ok"}` {
		t.Fatalf("boxd proxy response = %d %q, want HTTP 200 and health JSON", response.Code, response.Body.String())
	}
}

func TestToolsImageWarningIsLoggedOnce(t *testing.T) {
	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousWriter) })
	service := New(Config{}, dockercli.New(testExecutor{}), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	service.warnToolsUnavailable("tools image is unavailable")
	service.warnToolsUnavailable("tools image is still unavailable")
	if got := strings.Count(logs.String(), "tools image"); got != 1 {
		t.Fatalf("tools warning count = %d, want exactly one: %s", got, logs.String())
	}
}

func TestCreateRequestToolsImageOverridesNodeDefault(t *testing.T) {
	service := New(Config{ToolsImage: "images.example.test/boxes/tools:old"}, dockercli.New(testExecutor{}), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	if got := service.toolsImage(createRequest{ToolsImage: "images.example.test/boxes/tools:new"}); got != "images.example.test/boxes/tools:new" {
		t.Fatalf("tools image = %q, want image selected by the server", got)
	}
}

func TestPutStoragePersistsSecureConfigurationAndUpdatesRestic(t *testing.T) {
	storagePath := filepath.Join(t.TempDir(), "storage.json")
	resticCalls := []callRecord{}
	restic := testExecutor{calls: &resticCalls}
	engine := snapshot.New(testExecutor{}, restic, "/var/lib/boxes/restic", "old-password")
	service := New(Config{StoragePath: storagePath}, dockercli.New(testExecutor{}), engine)
	request := httptest.NewRequest(http.MethodPut, "/v1/storage", strings.NewReader(`{"repository":"s3:https://s3.example.test/boxes/prefix","env":{"AWS_ACCESS_KEY_ID":"access-value","AWS_SECRET_ACCESS_KEY":"secret-value","RESTIC_PASSWORD":"shared-password"}}`))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("PUT /v1/storage status = %d, body %s", response.Code, response.Body.String())
	}
	if engine.Repo != "s3:https://s3.example.test/boxes/prefix" || engine.Password != "shared-password" {
		t.Fatalf("restic engine repository/password = %q/%q", engine.Repo, engine.Password)
	}
	info, err := os.Stat(storagePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("storage file mode = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(storagePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "AWS_ACCESS_KEY_ID") || !strings.Contains(string(data), "access-value") {
		t.Fatalf("persisted storage file is missing the S3 environment: %s", data)
	}
	repository, password, env, err := LoadStorage(storagePath)
	if err != nil || repository != "s3:https://s3.example.test/boxes/prefix" || password != "shared-password" || env["AWS_SECRET_ACCESS_KEY"] != "secret-value" {
		t.Fatalf("LoadStorage() = %q, %q, %#v, %v", repository, password, env, err)
	}
	if len(resticCalls) != 1 || !strings.Contains(strings.Join(resticCalls[0].argv, " "), "cat config") {
		t.Fatalf("restic init check calls = %#v", resticCalls)
	}
}

func TestPutStorageSerializesInitPerRepository(t *testing.T) {
	var active, maxActive int32
	restic := testExecutor{command: func(_ []string, _ []byte, _ io.Writer, _ io.Writer) (int, error) {
		current := atomic.AddInt32(&active, 1)
		for previous := atomic.LoadInt32(&maxActive); current > previous; previous = atomic.LoadInt32(&maxActive) {
			if atomic.CompareAndSwapInt32(&maxActive, previous, current) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return 0, nil
	}}
	service := New(Config{StoragePath: filepath.Join(t.TempDir(), "storage.json")}, dockercli.New(testExecutor{}),
		snapshot.New(testExecutor{}, restic, "/old", "old-password"))
	var responses [2]*httptest.ResponseRecorder
	var workers sync.WaitGroup
	for index := range responses {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			request := httptest.NewRequest(http.MethodPut, "/v1/storage", strings.NewReader(
				`{"repository":"/same/repository","env":{"RESTIC_PASSWORD":"shared-password"}}`))
			responses[index] = httptest.NewRecorder()
			service.Handler().ServeHTTP(responses[index], request)
		}(index)
	}
	workers.Wait()
	for index, response := range responses {
		if response.Code != http.StatusNoContent {
			t.Fatalf("PUT /v1/storage response %d = %d: %s", index, response.Code, response.Body.String())
		}
	}
	if got := atomic.LoadInt32(&maxActive); got != 1 {
		t.Fatalf("concurrent restic init calls for one repository = %d, want 1", got)
	}
}

func TestPutStorageInitializesDifferentRepositoriesConcurrently(t *testing.T) {
	var active, maxActive int32
	restic := testExecutor{command: func(_ []string, _ []byte, _ io.Writer, _ io.Writer) (int, error) {
		current := atomic.AddInt32(&active, 1)
		for previous := atomic.LoadInt32(&maxActive); current > previous; previous = atomic.LoadInt32(&maxActive) {
			if atomic.CompareAndSwapInt32(&maxActive, previous, current) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return 0, nil
	}}
	service := New(Config{StoragePath: filepath.Join(t.TempDir(), "storage.json")}, dockercli.New(testExecutor{}),
		snapshot.New(testExecutor{}, restic, "/old", "old-password"))
	var responses [2]*httptest.ResponseRecorder
	var workers sync.WaitGroup
	for index, repository := range []string{"/repository/one", "/repository/two"} {
		workers.Add(1)
		go func(index int, repository string) {
			defer workers.Done()
			body := `{"repository":"` + repository + `","env":{"RESTIC_PASSWORD":"shared-password"}}`
			request := httptest.NewRequest(http.MethodPut, "/v1/storage", strings.NewReader(body))
			responses[index] = httptest.NewRecorder()
			service.Handler().ServeHTTP(responses[index], request)
		}(index, repository)
	}
	workers.Wait()
	for index, response := range responses {
		if response.Code != http.StatusNoContent {
			t.Fatalf("PUT /v1/storage response %d = %d: %s", index, response.Code, response.Body.String())
		}
	}
	if got := atomic.LoadInt32(&maxActive); got < 2 {
		t.Fatalf("concurrent restic init calls for different repositories = %d, want at least 2", got)
	}
}

func TestBoxdEventsProxyPreservesQueryAndEventResponseShape(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:7070")
	if err != nil {
		t.Skipf("boxd proxy test port is unavailable: %v", err)
	}
	defer listener.Close()
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" || r.URL.Query().Get("after") != "0" || r.URL.Query().Get("wait") != "25" {
			t.Errorf("upstream request = %s %s, want /events?after=0&wait=25", r.Method, r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer boxd-events-token" {
			t.Errorf("upstream authorization = %q, want boxd token", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"events":[{"cursor":7,"id":"ev_7","type":"turn.completed","time":"2026-09-24T12:00:00Z","data":{"runId":"run_1"}}],"cursor":7}`)
	}))
	upstream.Listener = listener
	upstream.Start()
	defer upstream.Close()

	const boxID = "bx_boxevntabcea"
	docker := dockercli.New(testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) >= 3 && argv[1] == "inspect" && argv[2] == "box-"+boxID {
			_, _ = io.WriteString(stdout, `[{"Id":"container","Name":"/box-`+boxID+`","State":{"Status":"running"},"NetworkSettings":{"Networks":{"boxes-net":{"IPAddress":"127.0.0.1"}}}}]`)
			return 0, nil
		}
		return 1, errors.New("unexpected Docker command")
	}})
	service := New(Config{NodeID: "nd_test", Runtime: "runc"}, docker, snapshot.New(testExecutor{}, testExecutor{}, "/repo", "password"))
	service.boxdTokens[boxID] = "boxd-events-token"
	service.desktopPasswords[boxID] = "desktop-events-password"
	request := httptest.NewRequest(http.MethodGet, "/v1/boxes/"+boxID+"/boxd/events?after=0&wait=25", nil)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("boxd events proxy status = %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Events []struct {
			Cursor int64          `json:"cursor"`
			ID     string         `json:"id"`
			Type   string         `json:"type"`
			Data   map[string]any `json:"data"`
		} `json:"events"`
		Cursor int64 `json:"cursor"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode proxied boxd events: %v", err)
	}
	if len(result.Events) != 1 || result.Events[0].Cursor != 7 || result.Events[0].ID != "ev_7" || result.Events[0].Type != "turn.completed" || result.Cursor != 7 {
		t.Fatalf("proxied events response = %#v, want event cursor 7 and outer cursor 7", result)
	}
}

func TestHealthReportsEmptyNodeIDWhenNotConfigured(t *testing.T) {
	service := New(Config{}, dockercli.New(testExecutor{}), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "password"))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	var health struct {
		NodeID string `json:"nodeId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if health.NodeID != "" {
		t.Fatalf("health nodeId = %q, want empty", health.NodeID)
	}
}

func TestCreateRestoresBeforeStartingAndReturnsContainerID(t *testing.T) {
	t.Setenv("BOXES_TOOLS_IMAGE", "none")
	var calls []callRecord
	startBoxdTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	archive := testTar(t, "restored.txt", "restored")
	inspectCount := 0
	dockerExec := testExecutor{calls: &calls, command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		switch {
		case reflect.DeepEqual(argv[:3], []string{"docker", "inspect", "box-bx_abcdefgh2345"}):
			inspectCount++
			if inspectCount == 1 {
				return 1, nil
			}
			_, _ = io.WriteString(stdout, `[{"Id":"container-123","Name":"/box-bx_abcdefgh2345","State":{"Status":"running"},"NetworkSettings":{"Networks":{"boxes-net":{"IPAddress":"127.0.0.1"}}}}]`)
			return 0, nil
		case argv[1] == "create":
			_, _ = io.WriteString(stdout, "container-123\n")
			return 0, nil
		default:
			return 0, nil
		}
	}}
	resticExec := testExecutor{calls: &calls, command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		for _, arg := range argv {
			if arg == "dump" {
				_, _ = stdout.Write(archive)
				break
			}
		}
		return 0, nil
	}}
	docker := dockercli.New(dockerExec)
	engine := snapshot.New(testExecutor{calls: &calls}, resticExec, "/repo", "restic-password")
	agent := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, docker, engine)
	request := httptest.NewRequest(http.MethodPost, "/v1/boxes", strings.NewReader(`{"boxId":"bx_abcdefgh2345","image":"image:tag","type":"small","fromSnapshot":"snapshot-123","fork":true,"desktopPassword":"desktop","boxdToken":"token"}`))
	response := httptest.NewRecorder()
	agent.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result struct {
		ContainerID string `json:"containerId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.ContainerID != "container-123" {
		t.Fatalf("response = %s, err = %v", response.Body.String(), err)
	}
	var createIndex, copyIndex, markerIndex, startIndex = -1, -1, -1, -1
	for i, call := range calls {
		if len(call.argv) >= 2 && call.argv[0] == "docker" {
			switch call.argv[1] {
			case "create":
				createIndex = i
				if strings.Contains(strings.Join(call.argv, " "), "/opt/boxes-tools") {
					t.Fatalf("disabled tools image unexpectedly mounted into box: %#v", call.argv)
				}
			case "cp":
				copyIndex = i
				reader := tar.NewReader(bytes.NewReader(call.stdin))
				for {
					header, err := reader.Next()
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						t.Fatalf("read copied archive: %v", err)
					}
					if header.Name == ".boxes/fork" {
						markerIndex = i
						break
					}
				}
			case "start":
				startIndex = i
			}
		}
	}
	if !(createIndex >= 0 && createIndex < copyIndex && copyIndex < startIndex && markerIndex >= 0 && markerIndex < startIndex) {
		t.Fatalf("expected docker create -> snapshot restore and fork marker -> docker start, calls = %#v", calls)
	}
}

func TestCreateRecoversOnceFromCorruptImageContent(t *testing.T) {
	t.Setenv("BOXES_TOOLS_IMAGE", "none")
	startBoxdTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	var calls []callRecord
	inspectCount, createCount := 0, 0
	const image = "images.example.test/boxes/box:dev"
	dockerExec := testExecutor{calls: &calls, command: func(argv []string, _ []byte, stdout, stderr io.Writer) (int, error) {
		if len(argv) < 2 || argv[0] != "docker" {
			return 1, errors.New("unexpected command")
		}
		switch argv[1] {
		case "inspect":
			if len(argv) > 2 && argv[2] == "box-bx_abcdefgh2345" {
				inspectCount++
				if inspectCount == 1 {
					return 1, nil
				}
				_, _ = io.WriteString(stdout, `[{"Id":"container-id","Name":"/box-bx_abcdefgh2345","State":{"Status":"running"},"NetworkSettings":{"Networks":{"boxes-net":{"IPAddress":"127.0.0.1"}}}}]`)
			}
		case "create":
			createCount++
			if createCount == 1 {
				_, _ = io.WriteString(stderr, "descriptor is neither a manifest or index PASSWORD=credential-value")
				return 1, nil
			}
			_, _ = io.WriteString(stdout, "container-id\n")
		case "start":
		case "rmi", "pull":
		case "image":
			if len(argv) < 3 || (argv[2] != "inspect" && argv[2] != "prune") {
				return 1, errors.New("unexpected Docker image command")
			}
		default:
			return 1, errors.New("unexpected Docker command")
		}
		return 0, nil
	}}
	service := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, dockercli.New(dockerExec), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))

	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousWriter) })
	request := httptest.NewRequest(http.MethodPost, "/v1/boxes", strings.NewReader(`{"boxId":"bx_abcdefgh2345","image":"`+image+`","type":"small"}`))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	if createCount != 2 {
		t.Fatalf("docker create calls = %d, want initial create and one retry", createCount)
	}

	var lifecycleCalls [][]string
	for _, call := range calls {
		if len(call.argv) > 1 && call.argv[0] == "docker" {
			switch call.argv[1] {
			case "create", "rmi", "image", "pull":
				lifecycleCalls = append(lifecycleCalls, call.argv)
			}
		}
	}
	want := [][]string{
		{"docker", "image", "inspect", image},
		{"docker", "create", "--name", "box-bx_abcdefgh2345", "--hostname", "bx_abcdefgh2345", "--runtime", "runc", "--label", "boxes.managed=true", "--label", "boxes.box-id=bx_abcdefgh2345", "--cpus", "2", "--memory", "4294967296", "--memory-reservation", "268435456", "--pids-limit", "4096", "--shm-size", "1g", "--network", "boxes-net", "--restart", "unless-stopped", "-e", "PUID=1000", "-e", "PGID=1000", "-e", "TZ=Etc/UTC", "-e", "PASSWORD=", "-e", "BOXD_TOKEN=", image},
		{"docker", "rmi", "-f", image},
		{"docker", "image", "prune", "-f"},
		{"docker", "pull", image},
		{"docker", "create", "--name", "box-bx_abcdefgh2345", "--hostname", "bx_abcdefgh2345", "--runtime", "runc", "--label", "boxes.managed=true", "--label", "boxes.box-id=bx_abcdefgh2345", "--cpus", "2", "--memory", "4294967296", "--memory-reservation", "268435456", "--pids-limit", "4096", "--shm-size", "1g", "--network", "boxes-net", "--restart", "unless-stopped", "-e", "PUID=1000", "-e", "PGID=1000", "-e", "TZ=Etc/UTC", "-e", "PASSWORD=", "-e", "BOXD_TOKEN=", image},
	}
	if !reflect.DeepEqual(lifecycleCalls, want) {
		t.Fatalf("image recovery calls = %#v, want %#v", lifecycleCalls, want)
	}
	logged := logs.String()
	for _, step := range []string{"remove_image", "prune_dangling_images", "pull_image", "retry_create"} {
		if !strings.Contains(logged, "step="+step) || !strings.Contains(logged, "descriptor is neither a manifest or index") {
			t.Errorf("recovery log is missing step %q and sanitized triggering error: %s", step, logged)
		}
	}
	if strings.Contains(logged, "credential-value") {
		t.Fatalf("recovery logs leaked credential value: %s", logged)
	}
}

func TestCreateRecoversFromCorruptInitialImagePull(t *testing.T) {
	t.Setenv("BOXES_TOOLS_IMAGE", "none")
	startBoxdTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	var calls []callRecord
	inspectCount, pullCount := 0, 0
	const image = "images.example.test/boxes/box:dev"
	dockerExec := testExecutor{calls: &calls, command: func(argv []string, _ []byte, stdout, stderr io.Writer) (int, error) {
		if len(argv) < 2 || argv[0] != "docker" {
			return 1, errors.New("unexpected command")
		}
		switch argv[1] {
		case "image":
			if len(argv) > 2 && argv[2] == "inspect" {
				return 1, nil
			}
		case "inspect":
			inspectCount++
			if inspectCount == 1 {
				return 1, nil
			}
			_, _ = io.WriteString(stdout, `[{"Id":"container-id","Name":"/box-bx_abcdefgh2345","State":{"Status":"running"},"NetworkSettings":{"Networks":{"boxes-net":{"IPAddress":"127.0.0.1"}}}}]`)
		case "pull":
			pullCount++
			if pullCount == 1 {
				_, _ = io.WriteString(stderr, "unexpected EOF while pulling image")
				return 1, nil
			}
		case "create":
			_, _ = io.WriteString(stdout, "container-id\n")
		case "rmi", "start":
		default:
			return 1, errors.New("unexpected Docker command")
		}
		return 0, nil
	}}
	service := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, dockercli.New(dockerExec), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	request := httptest.NewRequest(http.MethodPost, "/v1/boxes", strings.NewReader(`{"boxId":"bx_abcdefgh2345","image":"`+image+`","type":"small"}`))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	var recoveryCalls [][]string
	for _, call := range calls {
		if len(call.argv) > 1 && call.argv[0] == "docker" {
			switch call.argv[1] {
			case "image", "pull", "rmi", "create":
				recoveryCalls = append(recoveryCalls, call.argv)
			}
		}
	}
	want := [][]string{
		{"docker", "image", "inspect", image},
		{"docker", "pull", image},
		{"docker", "rmi", "-f", image},
		{"docker", "image", "prune", "-f"},
		{"docker", "pull", image},
		{"docker", "create", "--name", "box-bx_abcdefgh2345", "--hostname", "bx_abcdefgh2345", "--runtime", "runc", "--label", "boxes.managed=true", "--label", "boxes.box-id=bx_abcdefgh2345", "--cpus", "2", "--memory", "4294967296", "--memory-reservation", "268435456", "--pids-limit", "4096", "--shm-size", "1g", "--network", "boxes-net", "--restart", "unless-stopped", "-e", "PUID=1000", "-e", "PGID=1000", "-e", "TZ=Etc/UTC", "-e", "PASSWORD=", "-e", "BOXD_TOKEN=", image},
	}
	if !reflect.DeepEqual(recoveryCalls, want) {
		t.Fatalf("initial pull recovery calls = %#v, want %#v", recoveryCalls, want)
	}
}

func TestIsImageContentCorruptionRecognizesDockerContentErrors(t *testing.T) {
	tests := []struct {
		message string
		want    bool
	}{
		{message: "descriptor is neither a manifest or index", want: true},
		{message: "unexpected media type application/vnd.oci.image.layer.v1.tar", want: true},
		{message: "failed to unpack image sha256:abcd", want: true},
		{message: "content digest sha256:abcd not found", want: true},
		{message: "unexpected EOF during pull", want: true},
		{message: "permission denied while pulling image", want: false},
		{message: "content digest sha256:abcd is valid", want: false},
	}
	for _, test := range tests {
		t.Run(test.message, func(t *testing.T) {
			if got := isImageContentCorruption(errors.New(test.message)); got != test.want {
				t.Errorf("isImageContentCorruption(%q) = %v, want %v", test.message, got, test.want)
			}
		})
	}
	if isImageContentCorruption(nil) {
		t.Fatal("isImageContentCorruption(nil) = true, want false")
	}
}

func TestCreateReturnsConflictWhenContainerAlreadyExists(t *testing.T) {
	var dockerCalls []callRecord
	dockerExec := testExecutor{calls: &dockerCalls, command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) > 1 && argv[1] == "inspect" {
			_, _ = io.WriteString(stdout, `[{"Id":"existing","Name":"/box-bx_abcdefgh2345","State":{"Status":"running"}}]`)
			return 0, nil
		}
		return 0, nil
	}}
	agent := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, dockercli.New(dockerExec), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	request := httptest.NewRequest(http.MethodPost, "/v1/boxes", strings.NewReader(`{"boxId":"bx_abcdefgh2345","image":"image:tag","type":"small"}`))
	response := httptest.NewRecorder()
	agent.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
	}
}

func TestStopWithSnapshotFailureKeepsContainerRunningAndReturnsConflict(t *testing.T) {
	var dockerCalls []callRecord
	dockerExec := testExecutor{calls: &dockerCalls, command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) > 1 {
			switch argv[1] {
			case "diff":
				_, _ = io.WriteString(stdout, "A /file\n")
			case "inspect":
				_, _ = io.WriteString(stdout, "null")
			}
		}
		return 0, nil
	}}
	resticExec := testExecutor{command: func([]string, []byte, io.Writer, io.Writer) (int, error) { return 1, errors.New("capture failed") }}
	agent := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, dockercli.New(dockerExec), snapshot.New(dockerExec, resticExec, "/repo", "pw"))
	request := httptest.NewRequest(http.MethodPost, "/v1/boxes/bx_abcdefgh2345/stop", nil)
	response := httptest.NewRecorder()
	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousWriter) })
	agent.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "capture failed") {
		t.Fatalf("snapshot error detail is missing from response: %s", response.Body.String())
	}
	if !strings.Contains(logs.String(), "method=POST path=/v1/boxes/bx_abcdefgh2345/stop") || !strings.Contains(logs.String(), "capture failed") {
		t.Fatalf("failed stop operation was not logged with its error: %s", logs.String())
	}
	for _, call := range dockerCalls {
		if len(call.argv) > 1 && call.argv[1] == "stop" {
			t.Fatal("container was stopped after snapshot capture failed")
		}
	}
}

func TestCaptureSnapshotIfChangedSkipsMatchingFingerprint(t *testing.T) {
	proc := t.TempDir()
	upper := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proc, "123"), 0700); err != nil {
		t.Fatal(err)
	}
	mountInfo := "1 0 0:1 / / rw - overlay overlay rw,upperdir=" + upper + "\n"
	if err := os.WriteFile(filepath.Join(proc, "123", "mountinfo"), []byte(mountInfo), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOXES_HOST_PROC", proc)
	const boxID = "bx_abcdefgh2345"
	var resticCalls atomic.Int32
	dockerExec := testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) >= 4 && argv[1] == "inspect" && argv[2] == "-f" {
			_, _ = io.WriteString(stdout, "123\n")
			return 0, nil
		}
		if len(argv) >= 3 && argv[1] == "inspect" && argv[2] == "--format" {
			_, _ = io.WriteString(stdout, "{}\n")
			return 0, nil
		}
		return 1, fmt.Errorf("unexpected docker call: %v", argv)
	}}
	resticExec := testExecutor{command: func([]string, []byte, io.Writer, io.Writer) (int, error) { resticCalls.Add(1); return 0, nil }}
	engine := snapshot.New(dockerExec, resticExec, "/repo", "pw")
	agent := New(Config{}, dockercli.New(dockerExec), engine)
	fingerprint, err := engine.Fingerprint(context.Background(), boxID)
	if err != nil {
		t.Fatal(err)
	}
	agent.snapshotFingerprints[boxID] = fingerprint
	request := httptest.NewRequest(http.MethodPost, "/v1/boxes/"+boxID+"/snapshot", strings.NewReader(`{"ifChanged":true}`))
	response := httptest.NewRecorder()
	agent.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"skipped":true}` {
		t.Fatalf("response = %d %q, want skipped", response.Code, response.Body.String())
	}
	if resticCalls.Load() != 0 {
		t.Fatalf("restic calls = %d, want none", resticCalls.Load())
	}
}

func TestForgetSerializesExclusiveResticOperationsPerAgent(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	resticExec := testExecutor{command: func([]string, []byte, io.Writer, io.Writer) (int, error) {
		entered <- struct{}{}
		<-release
		return 0, nil
	}}
	service := New(Config{}, dockercli.New(testExecutor{}), snapshot.New(testExecutor{}, resticExec, "/repo", "pw"))
	serveForget := func() <-chan int {
		done := make(chan int, 1)
		go func() {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodDelete, "/v1/snapshots/snapshot-123", nil)
			service.Handler().ServeHTTP(response, request)
			done <- response.Code
		}()
		return done
	}

	service.snapshotExclusiveMu.RLock()
	firstDone := serveForget()
	select {
	case <-entered:
		service.snapshotExclusiveMu.RUnlock()
		t.Fatal("exclusive forget overlapped an active snapshot operation")
	case <-time.After(50 * time.Millisecond):
	}
	service.snapshotExclusiveMu.RUnlock()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first forget did not reach restic")
	}
	secondDone := serveForget()
	select {
	case <-entered:
		t.Fatal("second exclusive restic operation overlapped the first")
	case <-time.After(50 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("second forget did not start after the first completed")
	}
	release <- struct{}{}
	for _, done := range []<-chan int{firstDone, secondDone} {
		select {
		case status := <-done:
			if status != http.StatusNoContent {
				t.Errorf("forget response status = %d, want %d", status, http.StatusNoContent)
			}
		case <-time.After(time.Second):
			t.Fatal("forget request did not complete")
		}
	}
}

func TestForgetReportsUnreachableRepositoryWithSafeCode(t *testing.T) {
	resticExec := testExecutor{command: func(_ []string, _ []byte, _ io.Writer, stderr io.Writer) (int, error) {
		_, _ = io.WriteString(stderr, "Fatal: wrong password or no key found for s3:https://user:secret@example.test/repo")
		return 1, nil
	}}
	service := New(Config{}, dockercli.New(testExecutor{}), snapshot.New(testExecutor{}, resticExec, "s3:https://user:secret@example.test/repo", "pw"))
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/v1/snapshots/snapshot-123", nil)
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), `"code":"repository_unreachable"`) || strings.Contains(response.Body.String(), "secret@example.test") {
		t.Fatalf("response = %d %q, want safe repository_unreachable error", response.Code, response.Body.String())
	}
}

func TestStopReturnsSnapshotIDInServerContract(t *testing.T) {
	dockerExec := testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) > 1 && argv[1] == "inspect" {
			_, _ = io.WriteString(stdout, `{}`)
		} else if len(argv) > 1 && argv[1] == "exec" && containsArg(argv, "tar") {
			_, _ = io.WriteString(stdout, "tar-data")
		}
		return 0, nil
	}}
	resticExec := testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if containsArg(argv, "backup") {
			_, _ = io.WriteString(stdout, `{"snapshot_id":"restic-snapshot","total_bytes_processed":12}`)
		}
		return 0, nil
	}}
	agent := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, dockercli.New(dockerExec), snapshot.New(dockerExec, resticExec, "/repo", "pw"))
	request := httptest.NewRequest(http.MethodPost, "/v1/boxes/bx_abcdefgh2345/stop", nil)
	response := httptest.NewRecorder()
	agent.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("stop status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	var result struct {
		Snapshot struct {
			SnapshotID string `json:"snapshotId"`
		} `json:"snapshot"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode stop response: %v", err)
	}
	if result.Snapshot.SnapshotID != "restic-snapshot" {
		t.Fatalf("stop snapshot ID = %q, want restic-snapshot", result.Snapshot.SnapshotID)
	}
}

func TestRoutesWritesPrivateAtomicFileWithHTTPSAndHTTPRedirect(t *testing.T) {
	dir := t.TempDir()
	const forwardAuthURL = "http://agent:7001/auth/forward"
	agent := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: dir, ForwardAuthURL: forwardAuthURL, TraefikCheckAddr: "none"}, routesDocker(), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	body := `{"desktop":{"host":"desk.example.test","tokenHash":"hash","expiresAt":"2030-01-01T00:00:00Z"},"hosts":[{"host":"app.example.test","port":8080,"private":true,"tokenHash":"host-hash"}]}`
	request := httptest.NewRequest(http.MethodPut, "/v1/boxes/bx_abcdefgh2345/routes", strings.NewReader(body))
	response := httptest.NewRecorder()
	agent.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	path := filepath.Join(dir, "box-bx_abcdefgh2345.yml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatalf("route file must contain valid YAML: %v\n%s", err, data)
	}
	assertYAMLMapsNonEmpty(t, config, "root")
	encoded := string(data)
	for _, want := range []string{"websecure", "certResolver", "letsencrypt", forwardAuthURL, "addAuthCookiesToResponse", "web", "redirectScheme", "desk.example.test", "app.example.test"} {
		if !strings.Contains(encoded, want) {
			t.Errorf("route file does not contain %q: %s", want, encoded)
		}
	}
}

func TestRouteReadinessWaitsPastTraefik404(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "desk.example.test" || r.TLS.ServerName != "desk.example.test" {
			t.Errorf("host/SNI = %q/%q", r.Host, r.TLS.ServerName)
		}
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "404 page not found")
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "https://")
	if !waitRoutesReady(context.Background(), address, []string{"desk.example.test"}, time.Second, time.Millisecond) {
		t.Fatal("routes did not become ready after Traefik stopped returning 404")
	}
	if requests.Load() < 2 {
		t.Fatalf("probe count = %d, want at least 2", requests.Load())
	}
}

func TestRenderedRoutesNeverContainEmptyYAMLMaps(t *testing.T) {
	agent := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, routesDocker(), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	requests := []routesRequest{
		{},
		{Hosts: []hostRoute{{Host: "app.example.test", Port: 8080}}},
	}
	for _, request := range requests {
		content, _, err := agent.renderRoutes(context.Background(), "bx_abcdefgh2345", request)
		if err != nil {
			t.Fatalf("render routes: %v", err)
		}
		if len(content) == 0 {
			continue
		}
		var config map[string]any
		if err := yaml.Unmarshal(content, &config); err != nil {
			t.Fatalf("rendered routes must contain valid YAML: %v\n%s", err, content)
		}
		assertYAMLMapsNonEmpty(t, config, "root")
	}
}

func TestEmptyRoutesRequestDeletesExistingFileAndIgnoresMissingFile(t *testing.T) {
	for _, existing := range []bool{true, false} {
		t.Run(map[bool]string{true: "existing", false: "missing"}[existing], func(t *testing.T) {
			dir := t.TempDir()
			const boxID = "bx_abcdefgh2345"
			path := filepath.Join(dir, "box-"+boxID+".yml")
			if existing {
				if err := os.WriteFile(path, []byte("stale route config"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			agent := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: dir}, routesDocker(), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
			request := httptest.NewRequest(http.MethodPut, "/v1/boxes/"+boxID+"/routes", strings.NewReader(`{}`))
			response := httptest.NewRecorder()
			agent.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusNoContent {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("route file stat error = %v, want file removed", err)
			}
		})
	}
}

func assertYAMLMapsNonEmpty(t *testing.T, value any, path string) {
	t.Helper()
	switch value := value.(type) {
	case map[string]any:
		if len(value) == 0 {
			t.Errorf("rendered YAML contains empty map at %s", path)
			return
		}
		for key, nested := range value {
			assertYAMLMapsNonEmpty(t, nested, path+"."+key)
		}
	case []any:
		for index, nested := range value {
			assertYAMLMapsNonEmpty(t, nested, fmt.Sprintf("%s[%d]", path, index))
		}
	}
}

func TestCredentialsRecoverFromContainerAfterAgentRestart(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:7070")
	if err != nil {
		t.Skipf("boxd test port is unavailable: %v", err)
	}
	authorization := make(chan string, 1)
	upstream := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = upstream.Serve(listener) }()
	defer upstream.Close()

	var dockerCalls []callRecord
	dockerExec := testExecutor{calls: &dockerCalls, command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) > 1 && argv[1] == "inspect" {
			if len(argv) > 2 && argv[2] == "--format" {
				_, _ = io.WriteString(stdout, `["PASSWORD=desktop-from-container","BOXD_TOKEN=boxd-from-container"]`)
			} else {
				_, _ = io.WriteString(stdout, `[{"Id":"id","Name":"/box-bx_abcdefgh2345","State":{"Status":"running"},"NetworkSettings":{"Networks":{"boxes-net":{"IPAddress":"127.0.0.1"}}}}]`)
			}
		}
		return 0, nil
	}}
	agent := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, dockercli.New(dockerExec), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	const boxID = "bx_abcdefgh2345"

	routesRequest := httptest.NewRequest(http.MethodPut, "/v1/boxes/"+boxID+"/routes", strings.NewReader(`{"desktop":{"host":"desk.example.test","tokenHash":"hash"}}`))
	routesResponse := httptest.NewRecorder()
	agent.Handler().ServeHTTP(routesResponse, routesRequest)
	if routesResponse.Code != http.StatusNoContent {
		t.Fatalf("route setup status = %d, body = %s", routesResponse.Code, routesResponse.Body.String())
	}
	routeConfig, err := os.ReadFile(filepath.Join(agent.config.TraefikDir, "box-"+boxID+".yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(routeConfig), []byte("http:")) {
		t.Fatalf("Traefik route file is not YAML: %s", routeConfig)
	}
	wantDesktopAuthorization := "Basic " + base64.StdEncoding.EncodeToString([]byte("abc:desktop-from-container"))
	if !strings.Contains(string(routeConfig), wantDesktopAuthorization) {
		t.Fatalf("desktop route authorization does not use container password %q: %s", wantDesktopAuthorization, routeConfig)
	}

	proxyRequest := httptest.NewRequest(http.MethodGet, "/v1/boxes/"+boxID+"/boxd/status", nil)
	proxyResponse := httptest.NewRecorder()
	agent.Handler().ServeHTTP(proxyResponse, proxyRequest)
	if proxyResponse.Code != http.StatusNoContent {
		t.Fatalf("boxd proxy status = %d, body = %s", proxyResponse.Code, proxyResponse.Body.String())
	}
	select {
	case got := <-authorization:
		if got != "Bearer boxd-from-container" {
			t.Fatalf("boxd authorization = %q, want %q", got, "Bearer boxd-from-container")
		}
	default:
		t.Fatal("boxd request did not reach the container")
	}
	var envInspections int
	for _, call := range dockerCalls {
		if len(call.argv) > 2 && call.argv[1] == "inspect" && call.argv[2] == "--format" {
			envInspections++
		}
	}
	if envInspections != 1 {
		t.Fatalf("container environment inspected %d times, want one cached lookup", envInspections)
	}
}

func TestForwardAuthAcceptsQueryTokenAndSetsCookie(t *testing.T) {
	agent := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, routesDocker(), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	put := httptest.NewRequest(http.MethodPut, "/v1/boxes/bx_abcdefgh2345/routes", strings.NewReader(`{"desktop":{"host":"desk.example.test","tokenHash":"`+sha256Hex("access-token")+`","expiresAt":"2030-01-01T00:00:00Z"}}`))
	putResponse := httptest.NewRecorder()
	agent.Handler().ServeHTTP(putResponse, put)
	if putResponse.Code != http.StatusNoContent {
		t.Fatalf("route setup status = %d", putResponse.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/auth/forward", nil)
	request.Header.Set("X-Forwarded-Host", "desk.example.test:18443")
	request.Header.Set("X-Forwarded-Uri", "/?token=access-token")
	response := httptest.NewRecorder()
	agent.ForwardAuthHandler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	cookie := response.Result().Cookies()
	if len(cookie) != 1 || cookie[0].Name != "boxes_auth" || cookie[0].Value != "access-token" || !cookie[0].HttpOnly || !cookie[0].Secure || cookie[0].Domain != "" || cookie[0].MaxAge < 1 {
		t.Fatalf("auth cookies = %#v", cookie)
	}
}

func TestCreateCopiesBase64FilesWithRequestedMode(t *testing.T) {
	startBoxdTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	var dockerCalls []callRecord
	inspectCount := 0
	dockerExec := testExecutor{calls: &dockerCalls, command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) > 1 && argv[1] == "inspect" {
			inspectCount++
			if inspectCount == 1 {
				return 1, nil
			}
			_, _ = io.WriteString(stdout, `[{"Id":"container-id","Name":"/box-bx_abcdefgh2345","State":{"Status":"running"},"NetworkSettings":{"Networks":{"boxes-net":{"IPAddress":"127.0.0.1"}}}}]`)
			return 0, nil
		}
		if len(argv) > 1 && argv[1] == "create" {
			_, _ = io.WriteString(stdout, "container-id")
		}
		return 0, nil
	}}
	agent := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, dockercli.New(dockerExec), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	content := base64.StdEncoding.EncodeToString([]byte("file body"))
	request := httptest.NewRequest(http.MethodPost, "/v1/boxes", strings.NewReader(`{"boxId":"bx_abcdefgh2345","image":"image","type":"small","files":[{"path":"/config/setup.txt","mode":384,"content":"`+content+`"}]}`))
	response := httptest.NewRecorder()
	agent.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var copied bool
	for _, call := range dockerCalls {
		if reflect.DeepEqual(call.argv, []string{"docker", "cp", "-a", "-", "box-bx_abcdefgh2345:/"}) {
			copied = true
			reader := tar.NewReader(bytes.NewReader(call.stdin))
			header, err := reader.Next()
			if err != nil {
				t.Fatal(err)
			}
			if header.Name != "config/setup.txt" || header.Mode != 0o600 {
				t.Fatalf("file header = %#v", header)
			}
			got, err := io.ReadAll(reader)
			if err != nil || string(got) != "file body" {
				t.Fatalf("file content = %q, err = %v", got, err)
			}
		}
	}
	if !copied {
		t.Fatal("create did not copy uploaded files into the container")
	}
}

func TestCreateWaitsForBoxdHealth(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:7070")
	if err != nil {
		t.Skipf("boxd readiness test port is unavailable: %v", err)
	}
	healthChecks := 0
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthChecks++
		if r.Method != http.MethodGet || r.URL.Path != "/healthz" {
			t.Errorf("health request = %s %s, want GET /healthz", r.Method, r.URL.Path)
		}
		if healthChecks < 3 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	upstream.Listener = listener
	upstream.Start()
	defer upstream.Close()

	var inspects int
	dockerExec := testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) > 1 && argv[1] == "inspect" {
			inspects++
			if inspects == 1 {
				return 1, nil
			}
			_, _ = io.WriteString(stdout, `[{"Id":"container","Name":"/box-bx_abcdefgh2345","State":{"Status":"running"},"NetworkSettings":{"Networks":{"boxes-net":{"IPAddress":"127.0.0.1"}}}}]`)
		}
		if len(argv) > 1 && argv[1] == "create" {
			_, _ = io.WriteString(stdout, "container-id")
		}
		return 0, nil
	}}
	service := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, dockercli.New(dockerExec), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	request := httptest.NewRequest(http.MethodPost, "/v1/boxes", strings.NewReader(`{"boxId":"bx_abcdefgh2345","image":"image:tag","type":"small"}`))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	if healthChecks != 3 {
		t.Fatalf("boxd health checks = %d, want retries until the third (ready) response", healthChecks)
	}
}

func TestStartWaitsForBoxdHealth(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:7070")
	if err != nil {
		t.Skipf("boxd readiness test port is unavailable: %v", err)
	}
	healthChecks := 0
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthChecks++
		if r.Method != http.MethodGet || r.URL.Path != "/healthz" {
			t.Errorf("health request = %s %s, want GET /healthz", r.Method, r.URL.Path)
		}
		if healthChecks == 1 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	upstream.Listener = listener
	upstream.Start()
	defer upstream.Close()

	dockerExec := testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) > 1 && argv[1] == "inspect" {
			_, _ = io.WriteString(stdout, `[{"Id":"container","Name":"/box-bx_abcdefgh2345","State":{"Status":"running"},"NetworkSettings":{"Networks":{"boxes-net":{"IPAddress":"127.0.0.1"}}}}]`)
		}
		return 0, nil
	}}
	service := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, dockercli.New(dockerExec), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	request := httptest.NewRequest(http.MethodPost, "/v1/boxes/bx_abcdefgh2345/start", nil)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("start status = %d, want %d: %s", response.Code, http.StatusNoContent, response.Body.String())
	}
	if healthChecks != 2 {
		t.Fatalf("boxd health checks = %d, want retry until ready", healthChecks)
	}
}

func TestStartReturnsBoxNotReadyWhenHealthTimeoutExpires(t *testing.T) {
	startBoxdTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	dockerExec := testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) > 1 && argv[1] == "inspect" {
			_, _ = io.WriteString(stdout, `[{"Id":"container","Name":"/box-bx_abcdefgh2345","State":{"Status":"running"},"NetworkSettings":{"Networks":{"boxes-net":{"IPAddress":"127.0.0.1"}}}}]`)
		}
		return 0, nil
	}}
	service := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, dockercli.New(dockerExec), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/v1/boxes/bx_abcdefgh2345/start", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"box_not_ready"`) {
		t.Fatalf("start timeout response = %d %s, want box_not_ready", response.Code, response.Body.String())
	}
}

func TestPutFileAssignsUploadedFileToBoxUser(t *testing.T) {
	var dockerCalls []callRecord
	dockerExec := testExecutor{calls: &dockerCalls}
	agent := New(Config{NodeID: "nd_test"}, dockercli.New(dockerExec), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	request := httptest.NewRequest(http.MethodPut, "/v1/boxes/bx_abcdefgh2345/files?path=%2Fconfig%2F.local%2Fshare%2Fopencode%2Fauth.json&mode=0600", strings.NewReader("auth data"))
	response := httptest.NewRecorder()
	agent.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
	want := []string{"docker", "exec", "--user", "root", "box-bx_abcdefgh2345", "chown", "abc:abc", "--", "/config/.local/share/opencode/auth.json"}
	for _, call := range dockerCalls {
		if reflect.DeepEqual(call.argv, want) {
			return
		}
	}
	t.Fatalf("file upload did not assign file to abc; docker calls = %#v", dockerCalls)
}

func TestPutArchiveStreamsIntoBoxAsDefaultUser(t *testing.T) {
	var calls []callRecord
	dockerExec := testExecutor{calls: &calls}
	service := New(Config{NodeID: "nd_test"}, dockercli.New(dockerExec), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	request := httptest.NewRequest(http.MethodPut, "/v1/boxes/bx_abcdefgh2345/archive?path=%2Fconfig%2Frepo", strings.NewReader("gzip archive"))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
	if len(calls) != 1 {
		t.Fatalf("docker calls = %#v", calls)
	}
	want := []string{"docker", "exec", "--interactive", "--user", "1000:1000", "box-bx_abcdefgh2345", "sh", "-c", "mkdir -p -- '/config/repo' && tar -xzf - -C '/config/repo' --no-same-owner"}
	if !reflect.DeepEqual(calls[0].argv, want) || string(calls[0].stdin) != "gzip archive" {
		t.Fatalf("archive exec = %#v stdin %q", calls[0].argv, calls[0].stdin)
	}
}

func TestGetArtifactsStreamsDirectoryArchiveAsDefaultUser(t *testing.T) {
	var calls []callRecord
	executor := testExecutor{calls: &calls, command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "stat -L") {
			_, _ = io.WriteString(stdout, "directory\n")
		}
		if strings.Contains(joined, "tar czf -") {
			_, _ = io.WriteString(stdout, "archive bytes")
		}
		return 0, nil
	}}
	service := New(Config{NodeID: "nd_test"}, dockercli.New(executor), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	request := httptest.NewRequest(http.MethodGet, "/v1/boxes/bx_abcdefgh2345/artifacts?path=%2Fconfig%2Frepo", nil)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "archive bytes" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if len(calls) != 2 || !strings.Contains(strings.Join(calls[1].argv, " "), "--user 1000:1000") {
		t.Fatalf("artifact docker calls = %#v", calls)
	}
}

func TestPutArchiveRejectsInvalidPath(t *testing.T) {
	service := New(Config{NodeID: "nd_test"}, dockercli.New(testExecutor{}), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	request := httptest.NewRequest(http.MethodPut, "/v1/boxes/bx_abcdefgh2345/archive?path=relative", strings.NewReader("data"))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
}

func TestPutFileCreatesParentsAsBoxUserOnlyUnderConfig(t *testing.T) {
	tests := []struct {
		name string
		path string
		user string
	}{
		{name: "config root", path: "/config/auth.json", user: "1000:1000"},
		{name: "nested config", path: "/config/.local/share/opencode/auth.json", user: "1000:1000"},
		{name: "other path", path: "/workspace/auth.json", user: "root"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var dockerCalls []callRecord
			dockerExec := testExecutor{calls: &dockerCalls}
			service := New(Config{NodeID: "nd_test"}, dockercli.New(dockerExec), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
			request := httptest.NewRequest(http.MethodPut, "/v1/boxes/bx_abcdefgh2345/files?path="+url.QueryEscape(test.path), strings.NewReader("content"))
			response := httptest.NewRecorder()
			service.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusNoContent {
				t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
			}

			want := []string{"docker", "exec", "--user", test.user}
			if test.user == "1000:1000" {
				want = append(want, "--env", "HOME=/config", "--env", "USER=abc")
			}
			want = append(want, "box-bx_abcdefgh2345", "mkdir", "-p", "--", path.Dir(test.path))
			for _, call := range dockerCalls {
				if reflect.DeepEqual(call.argv, want) {
					return
				}
			}
			t.Fatalf("mkdir command did not use user %q; docker calls = %#v", test.user, dockerCalls)
		})
	}
}

func TestExecExecutorUsesArgumentsWithoutShellInterpretation(t *testing.T) {
	printf, err := exec.LookPath("printf")
	if err != nil {
		t.Skip("printf is unavailable")
	}
	var stdout, stderr bytes.Buffer
	code, err := (ExecExecutor{}).Run(context.Background(), []string{printf, "%s|%s", "hello world", "x;touch /tmp/boxes-shell-must-not-run"}, nil, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("Run() = (%d, %v), stderr %q", code, err, stderr.String())
	}
	if got, want := stdout.String(), "hello world|x;touch /tmp/boxes-shell-must-not-run"; got != want {
		t.Fatalf("stdout = %q, want literal argv output %q", got, want)
	}
}

func TestExecAPIUsesArgvAndDefaultUserAndWorkingDirectory(t *testing.T) {
	var dockerCalls []callRecord
	dockerExec := testExecutor{calls: &dockerCalls, command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) > 1 && argv[1] == "inspect" {
			return 1, nil
		}
		if stdout != nil {
			_, _ = io.WriteString(stdout, "ran")
		}
		return 7, nil
	}}
	agent := New(Config{NodeID: "nd_test"}, dockercli.New(dockerExec), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	request := httptest.NewRequest(http.MethodPost, "/v1/boxes/bx_abcdefgh2345/exec", strings.NewReader(`{"cmd":["printf","%s","hello world"]}`))
	response := httptest.NewRecorder()
	agent.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"exitCode":7`) || !strings.Contains(response.Body.String(), `"stdout":"ran"`) {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
	var execCall []string
	for _, call := range dockerCalls {
		if len(call.argv) > 1 && call.argv[1] == "exec" {
			execCall = call.argv
		}
	}
	want := []string{"docker", "exec", "--user", "1000:1000", "--workdir", "/config", "--env", "HOME=/config", "--env", "USER=abc", "box-bx_abcdefgh2345", "printf", "%s", "hello world"}
	if !reflect.DeepEqual(execCall, want) {
		t.Fatalf("docker exec argv = %#v, want %#v", execCall, want)
	}
}

func testTar(t *testing.T, name, content string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	w := tar.NewWriter(&buffer)
	if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func startBoxdTestServer(t *testing.T, handler http.Handler) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:7070")
	if err != nil {
		t.Skipf("boxd test port is unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
}

func sha256Hex(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func containsArg(argv []string, want string) bool {
	for _, arg := range argv {
		if arg == want {
			return true
		}
	}
	return false
}

func routesDocker() *dockercli.Docker {
	executor := testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) > 1 && argv[1] == "inspect" {
			if len(argv) > 2 && argv[2] == "--format" {
				_, _ = io.WriteString(stdout, `[]`)
			} else {
				_, _ = io.WriteString(stdout, `[{"Id":"id","Name":"/box-bx_abcdefgh2345","State":{"Status":"running"},"NetworkSettings":{"Networks":{"boxes-net":{"IPAddress":"172.18.0.3"}}}}]`)
			}
		}
		return 0, nil
	}}
	return dockercli.New(executor)
}

func TestBoxStatsUsesContainerName(t *testing.T) {
	const boxID = "bx_statsnamexyz"
	docker := dockercli.New(testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) >= 2 && argv[1] == "stats" {
			if argv[len(argv)-1] != "box-"+boxID {
				return 1, errors.New("docker stats called with " + argv[len(argv)-1])
			}
			_, _ = io.WriteString(stdout, `{"CPUPerc":"6.75%"}`)
			return 0, nil
		}
		if len(argv) >= 2 && argv[1] == "exec" {
			_, _ = io.WriteString(stdout, "  sl  local_address rem_address   st\n")
			return 0, nil
		}
		return 1, errors.New("unexpected Docker command")
	}})
	service := New(Config{}, docker, snapshot.New(testExecutor{}, testExecutor{}, "/repo", "password"))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/boxes/"+boxID+"/stats", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"cpuPercent":6.75`) {
		t.Fatalf("stats response = %d %q, want HTTP 200 with cpuPercent 6.75", response.Code, response.Body.String())
	}
}

func TestParseCommandRunsStringsThroughShell(t *testing.T) {
	argv, err := parseCommand(json.RawMessage(`"echo hi && go version | head -1"`))
	if err != nil || strings.Join(argv, "\x00") != "bash\x00-lc\x00echo hi && go version | head -1" {
		t.Fatalf("string cmd = %q, %v; want bash -lc <command>", argv, err)
	}
	argv, err = parseCommand(json.RawMessage(`["echo","a && b"]`))
	if err != nil || len(argv) != 2 || argv[1] != "a && b" {
		t.Fatalf("array cmd = %q, %v; want argv unchanged", argv, err)
	}
	if _, err := parseCommand(json.RawMessage(`"   "`)); err == nil {
		t.Fatal("blank cmd was accepted")
	}
}

// detachedRecorder is a mutex-guarded docker argv log for tests that drive detached commands over HTTP.
type detachedRecorder struct {
	mu   sync.Mutex
	argv [][]string
}

func (r *detachedRecorder) add(argv []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.argv = append(r.argv, append([]string(nil), argv...))
}

func (r *detachedRecorder) find(match func([]string) bool) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, argv := range r.argv {
		if match(argv) {
			return argv
		}
	}
	return nil
}

func isDetachedExec(argv []string) bool {
	return len(argv) > 1 && argv[1] == "exec" && containsArg(argv, "setsid")
}

func waitForProcessExit(t *testing.T, handler http.Handler, boxID, processID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/boxes/"+boxID+"/exec/"+processID, nil))
		if strings.Contains(response.Body.String(), `"running":false`) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("detached process did not finish")
}

func startDetached(t *testing.T, handler http.Handler, boxID, body string) string {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/boxes/"+boxID+"/exec", strings.NewReader(body)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("detached start = %d %s", response.Code, response.Body.String())
	}
	var started struct {
		ProcessID string `json:"processId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &started); err != nil || started.ProcessID == "" {
		t.Fatalf("processId missing from %s: %v", response.Body.String(), err)
	}
	return started.ProcessID
}

func TestOutputBufferKeepsTailAndOffsets(t *testing.T) {
	var buffer outputBuffer
	chunk := bytes.Repeat([]byte("a"), 256<<10)
	for i := 0; i < 12; i++ { // 3 MiB total
		_, _ = buffer.Write(chunk)
	}
	data, next, truncated := buffer.readFrom(0)
	if !truncated || len(data) != maxDetachedOutputBytes || next != 3<<20 {
		t.Fatalf("readFrom(0) = len %d next %d truncated %v; want 1 MiB tail, next 3 MiB, truncated", len(data), next, truncated)
	}
	data, next, truncated = buffer.readFrom(3<<20 - 5)
	if truncated || string(data) != "aaaaa" || next != 3<<20 {
		t.Fatalf("readFrom(end-5) = %q next %d truncated %v", data, next, truncated)
	}
	if _, _, truncated = buffer.readFrom(1); !truncated {
		t.Fatal("offset before the retained window was not reported as truncated")
	}
}

func TestDetachedLogsReturnDataAfterOffsetAndFollowEndsWithExit(t *testing.T) {
	release := make(chan struct{})
	var rec detachedRecorder
	executor := testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if !isDetachedExec(argv) {
			return 0, nil
		}
		rec.add(argv)
		_, _ = io.WriteString(stdout, "one\n")
		<-release
		_, _ = io.WriteString(stdout, "two\n")
		return 0, nil
	}}
	agent := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, dockercli.New(executor), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	server := httptest.NewServer(agent.Handler())
	defer server.Close()
	boxID := "bx_abcdefgh2345"
	processID := startDetached(t, agent.Handler(), boxID, `{"cmd":["printf","x"],"detached":true}`)

	// Live output: the first chunk is visible while the process is still running.
	var follow *http.Response
	done := make(chan string, 1)
	go func() {
		response, err := http.Get(server.URL + "/v1/boxes/" + boxID + "/exec/" + processID + "/logs?follow=1")
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		follow = response
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		done <- string(body)
	}()
	close(release)
	body := <-done
	if follow == nil || !strings.Contains(body, `"data":"one\ntwo\n","offset":8,"stream":"stdout"`) {
		t.Fatalf("follow body = %q", body)
	}
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if last := lines[len(lines)-1]; last != `{"exit":true,"exitCode":0}` {
		t.Fatalf("last follow line = %q", last)
	}

	// Non-follow after offset 4 returns only the bytes after it and no exit line.
	response := httptest.NewRecorder()
	agent.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/boxes/"+boxID+"/exec/"+processID+"/logs?stdoutOffset=4", nil))
	if response.Code != http.StatusOK || response.Body.String() != "{\"data\":\"two\\n\",\"offset\":8,\"stream\":\"stdout\"}\n" {
		t.Fatalf("non-follow after offset = %d %q", response.Code, response.Body.String())
	}
	if response := httptest.NewRecorder(); true {
		agent.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/boxes/"+boxID+"/exec/"+processID+"/logs?stdoutOffset=x", nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid offset = %d", response.Code)
		}
	}
}

func TestDetachedKillSignalsProcessGroupFromPidFile(t *testing.T) {
	release := make(chan struct{})
	var rec detachedRecorder
	executor := testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if isDetachedExec(argv) {
			<-release
			return 0, nil
		}
		rec.add(argv)
		if containsArg(argv, "cat") {
			_, _ = io.WriteString(stdout, "4321\n")
		}
		return 0, nil
	}}
	agent := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, dockercli.New(executor), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	boxID := "bx_abcdefgh2345"
	processID := startDetached(t, agent.Handler(), boxID, `{"cmd":["sleep","100"],"detached":true}`)
	kill := func(body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		agent.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/boxes/"+boxID+"/exec/"+processID+"/kill", strings.NewReader(body)))
		return response
	}
	if response := kill(`{"signal":"STOP"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("unsupported signal = %d", response.Code)
	}
	if response := kill(`{"signal":"HUP"}`); response.Code != http.StatusOK {
		t.Fatalf("kill HUP = %d %s", response.Code, response.Body.String())
	}
	want := []string{"docker", "exec", "--user", "root", "box-" + boxID, "kill", "-s", "HUP", "--", "-4321"}
	if got := rec.find(func(argv []string) bool { return containsArg(argv, "kill") }); !reflect.DeepEqual(got, want) {
		t.Fatalf("kill argv = %#v, want %#v", got, want)
	}
	close(release)
	waitForProcessExit(t, agent.Handler(), boxID, processID)
	if response := kill(`{}`); response.Code != http.StatusConflict {
		t.Fatalf("kill after exit = %d", response.Code)
	}
}

func TestDetachedArgvRecordsPidAndKeepsArgumentsLiteral(t *testing.T) {
	got := detachedArgv("abc_-1", []string{"printf", "%s", "a b;c"})
	want := []string{"setsid", "sh", "-c", `mkdir -p /tmp/.boxes-exec; echo $$ > "$0"; exec "$@"`, "/tmp/.boxes-exec/abc_-1.pid", "printf", "%s", "a b;c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("detachedArgv = %#v, want %#v", got, want)
	}
}

func TestDetachedStdinWriteCloseAndConflicts(t *testing.T) {
	var rec detachedRecorder
	executor := testExecutor{command: func(argv []string, input []byte, stdout, _ io.Writer) (int, error) {
		if isDetachedExec(argv) {
			_, _ = stdout.Write(input) // testExecutor reads stdin until it is closed, then calls this
			return 0, nil
		}
		rec.add(argv)
		return 0, nil
	}}
	agent := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, dockercli.New(executor), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	boxID := "bx_abcdefgh2345"
	stdinPost := func(processID, query, body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		agent.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/boxes/"+boxID+"/exec/"+processID+"/stdin"+query, strings.NewReader(body)))
		return response
	}
	if response := httptest.NewRecorder(); true {
		agent.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/boxes/"+boxID+"/exec", strings.NewReader(`{"cmd":["cat"],"stdin":true}`)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("sync stdin = %d", response.Code)
		}
	}
	noStdin := startDetached(t, agent.Handler(), boxID, `{"cmd":["true"],"detached":true}`)
	if response := stdinPost(noStdin, "", "x"); response.Code != http.StatusConflict {
		t.Fatalf("stdin without stdin flag = %d", response.Code)
	}
	processID := startDetached(t, agent.Handler(), boxID, `{"cmd":["cat"],"detached":true,"stdin":true}`)
	if response := stdinPost(processID, "", "hello "); response.Code != http.StatusOK {
		t.Fatalf("stdin write = %d %s", response.Code, response.Body.String())
	}
	if response := stdinPost(processID, "?close=1", "world"); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"closed":true`) {
		t.Fatalf("stdin close = %d %s", response.Code, response.Body.String())
	}
	waitForProcessExit(t, agent.Handler(), boxID, processID)
	if response := stdinPost(processID, "", "late"); response.Code != http.StatusConflict {
		t.Fatalf("write after close = %d", response.Code)
	}
	if response := stdinPost(processID, "", strings.Repeat("x", maxStdinWriteBytes+1)); response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized stdin = %d", response.Code)
	}
	response := httptest.NewRecorder()
	agent.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/boxes/"+boxID+"/exec/"+processID, nil))
	if !strings.Contains(response.Body.String(), `"stdout":"hello world"`) {
		t.Fatalf("process output = %s", response.Body.String())
	}
}

func TestDetachedTimeoutBounds(t *testing.T) {
	executor := testExecutor{command: func([]string, []byte, io.Writer, io.Writer) (int, error) { return 0, nil }}
	agent := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir()}, dockercli.New(executor), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	boxID := "bx_abcdefgh2345"
	post := func(body string) int {
		response := httptest.NewRecorder()
		agent.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/boxes/"+boxID+"/exec", strings.NewReader(body)))
		return response.Code
	}
	if code := post(`{"cmd":["true"],"timeoutSeconds":601}`); code != http.StatusBadRequest {
		t.Fatalf("sync 601s = %d", code)
	}
	if code := post(`{"cmd":["true"],"detached":true,"timeoutSeconds":86401}`); code != http.StatusBadRequest {
		t.Fatalf("detached 86401s = %d", code)
	}
	if code := post(`{"cmd":["true"],"detached":true,"timeoutSeconds":86400}`); code != http.StatusAccepted {
		t.Fatalf("detached 86400s = %d", code)
	}
	id := startDetached(t, agent.Handler(), boxID, `{"cmd":["true"],"detached":true}`)
	process := agent.findProcess(boxID, id)
	if remaining := time.Until(process.Deadline); remaining < 59*time.Minute || remaining > time.Hour+time.Minute {
		t.Fatalf("default detached deadline is %s away, want about 1h", remaining)
	}
}

func TestListPruningKeepsRunningAndRecentAndCapsBox(t *testing.T) {
	agent := New(Config{NodeID: "nd_test"}, dockercli.New(testExecutor{}), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	now := time.Now().UTC()
	finished := func(boxID string, age time.Duration) *execProcess {
		process := newExecProcess(boxID, []string{"true"}, time.Minute)
		process.finish(0)
		process.finishedAt = now.Add(-age)
		return process
	}
	running := newExecProcess("bx_a", []string{"sleep"}, time.Minute)
	agent.processes["running"] = running
	agent.processes["old"] = finished("bx_a", 2*time.Hour)
	agent.processes["recent"] = finished("bx_a", 30*time.Minute)
	for i := 0; i < 105; i++ {
		agent.processes[fmt.Sprintf("bulk-%03d", i)] = finished("bx_a", time.Duration(i+1)*time.Second)
	}
	agent.processes["other"] = finished("bx_b", 2*time.Hour)

	agent.mu.Lock()
	agent.pruneProcessesLocked("bx_a", now)
	agent.mu.Unlock()

	if _, ok := agent.processes["old"]; ok {
		t.Fatal("finished process older than one hour was kept")
	}
	if _, ok := agent.processes["running"]; !ok {
		t.Fatal("running process was dropped")
	}
	if _, ok := agent.processes["other"]; !ok {
		t.Fatal("another box's process was pruned")
	}
	count := 0
	for _, process := range agent.processes {
		if process.BoxID == "bx_a" {
			count++
		}
	}
	if count != maxProcessesPerBox {
		t.Fatalf("box has %d processes, want %d", count, maxProcessesPerBox)
	}
	if _, ok := agent.processes["bulk-000"]; !ok {
		t.Fatal("newest finished process was dropped")
	}
	if _, ok := agent.processes["bulk-104"]; ok {
		t.Fatal("oldest finished process was kept over the cap")
	}
}
