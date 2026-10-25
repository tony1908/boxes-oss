// Package agent implements the boxes node HTTP API.
package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/tony1908/boxes-oss/internal/diagnostics"
	"github.com/tony1908/boxes-oss/internal/dockercli"
	"github.com/tony1908/boxes-oss/internal/snapshot"
	boxtypes "github.com/tony1908/boxes-oss/internal/types"
)

const (
	defaultRuntime        = "sysbox-runc"
	defaultTraefikDir     = "/etc/boxes/traefik/dynamic"
	defaultForwardAuthURL = "http://127.0.0.1:7001/auth/forward"
	maxExecOutput         = 1 << 20
	maxRequestBody        = 16 << 20
	maxArchiveBody        = 2 << 30
	boxdReadyTimeout      = 180 * time.Second
	boxdPollInterval      = time.Second
)

var errBoxNotReady = errors.New("box_not_ready")

var boxIDPattern = regexp.MustCompile(`^bx_[a-z2-7]{12}$`)

// Config contains the agent's node-specific settings.
type Config struct {
	NodeID           string
	Runtime          string
	ToolsImage       string
	TraefikDir       string
	NetworkDir       string
	ForwardAuthURL   string
	StoragePath      string
	ProcRoot         string
	TraefikCheckAddr string
}

// Agent serves box lifecycle and snapshot operations on a node.
type Agent struct {
	config    Config
	docker    *dockercli.Docker
	snapshots *snapshot.Engine

	mu                   sync.RWMutex
	snapshotExclusiveMu  sync.RWMutex
	storageInitMu        sync.Mutex
	storageInitLocks     map[string]*sync.Mutex
	boxdTokens           map[string]string
	desktopPasswords     map[string]string
	forwardAuth          map[string]authCredential
	processes            map[string]*execProcess
	snapshotFingerprints map[string]string
	toolsWarning         sync.Once
	networkMu            sync.Mutex
	dnsHistory           map[string]map[string]map[string]time.Time // box -> domain -> IPv4 -> last resolved; guarded by networkMu
	dnsSources           map[string]dnsSource                       // container IP -> box policy, for DNS; guarded by networkMu
	dnsGateway           string                                     // DNS forwarder address boxes use; empty when not serving; guarded by networkMu
	dnsUpstream          []string                                   // host resolvers the forwarder asks; set once by StartDNS
	firewall             dockercli.Executor
	readFile             func(string) ([]byte, error) // host file reads for box usage; faked in tests
	usageWarned          sync.Map                     // box IDs whose usage read already failed and was logged
}

type authCredential struct {
	boxID     string
	hash      string
	expiresAt time.Time
}

type createRequest struct {
	BoxID           string                  `json:"boxId"`
	Image           string                  `json:"image"`
	ToolsImage      string                  `json:"toolsImage,omitempty"`
	Type            string                  `json:"type"`
	Runtime         string                  `json:"runtime"`
	Env             map[string]string       `json:"env"`
	Files           []boxFile               `json:"files"`
	FromSnapshot    string                  `json:"fromSnapshot"`
	Fork            bool                    `json:"fork"`
	DesktopPassword string                  `json:"desktopPassword"`
	BoxdToken       string                  `json:"boxdToken"`
	Storage         storageUpdate           `json:"storage"`
	NetworkPolicy   *boxtypes.NetworkPolicy `json:"networkPolicy,omitempty"`
	Mounts          []boxtypes.Mount        `json:"mounts,omitempty"`
}

type boxFile struct {
	Path    string `json:"path"`
	Mode    int64  `json:"mode"`
	Content string `json:"content"`
}

type execRequest struct {
	Cmd            json.RawMessage   `json:"cmd"`
	Cwd            string            `json:"cwd"`
	User           string            `json:"user"`
	Env            map[string]string `json:"env"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	Detached       bool              `json:"detached"`
	Stdin          bool              `json:"stdin"`
	Desktop        bool              `json:"desktop"`
}

// defaultExecCWD is the working directory for a command that does not set one: the default user's
// home is /config, and a user created with the users API starts in its own /home/<user>.
func defaultExecCWD(user string) string {
	if user == "" || user == "abc" || user == "root" {
		return "/config"
	}
	return "/home/" + user
}

var (
	errInvalidExecUser  = errors.New("invalid exec user")
	errExecUserNotFound = errors.New("exec user not found")
	execUserPattern     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

// prepareBoxUser resolves a request user other than abc and root. Docker cannot resolve accounts
// created at runtime, so such commands run as root behind the returned setpriv prefix. The user's
// home becomes the default cwd and HOME/USER/LOGNAME env. Returns nil for abc, root and "".
func (a *Agent) prepareBoxUser(ctx context.Context, container string, request *execRequest) ([]string, error) {
	user := request.User
	if user == "" || user == "abc" || user == "root" {
		return nil, nil
	}
	if !execUserPattern.MatchString(user) {
		return nil, errInvalidExecUser
	}
	var passwd bytes.Buffer
	code, err := a.docker.Exec(ctx, container, "root", "", nil, []string{"getent", "passwd", user}, nil, &passwd, io.Discard)
	if err != nil {
		return nil, err
	}
	if code == 2 { // getent: key not found
		return nil, errExecUserNotFound
	}
	fields := strings.Split(strings.TrimSpace(passwd.String()), ":")
	if code != 0 || len(fields) < 7 || fields[0] != user || !path.IsAbs(fields[5]) {
		return nil, fmt.Errorf("getent passwd %s returned code %d", user, code)
	}
	home := fields[5]
	if request.Cwd == "" {
		request.Cwd = home
	}
	env := make(map[string]string, len(request.Env)+3)
	for key, value := range request.Env {
		env[key] = value
	}
	env["HOME"], env["USER"], env["LOGNAME"] = home, user, user
	request.Env = env
	request.User = "root"
	// --init-groups keeps supplementary groups, such as a /shared/<group> membership.
	return []string{"setpriv", "--reuid=" + user, "--regid=" + user, "--init-groups", "--"}, nil
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// New creates an agent backed by the Docker CLI and Restic snapshot engine.
func New(config Config, docker *dockercli.Docker, snapshots *snapshot.Engine) *Agent {
	if config.Runtime == "" {
		config.Runtime = defaultRuntime
	}
	if config.TraefikDir == "" {
		config.TraefikDir = defaultTraefikDir
	}
	if config.NetworkDir == "" {
		config.NetworkDir = defaultNetworkDir
	}
	if config.ForwardAuthURL == "" {
		config.ForwardAuthURL = defaultForwardAuthURL
	}
	if config.StoragePath == "" {
		config.StoragePath = "/etc/boxes/storage.json"
	}
	if config.ToolsImage == "" {
		config.ToolsImage = os.Getenv("BOXES_TOOLS_IMAGE")
	}
	if config.TraefikCheckAddr == "" {
		config.TraefikCheckAddr = os.Getenv("BOXES_TRAEFIK_CHECK_ADDR")
	}
	if config.TraefikCheckAddr == "" {
		config.TraefikCheckAddr = "127.0.0.1:443"
	}
	return &Agent{
		config: config, docker: docker, snapshots: snapshots,
		boxdTokens: make(map[string]string), desktopPasswords: make(map[string]string), forwardAuth: make(map[string]authCredential),
		processes: make(map[string]*execProcess), snapshotFingerprints: make(map[string]string), storageInitLocks: make(map[string]*sync.Mutex),
		firewall: ExecExecutor{}, readFile: os.ReadFile,
	}
}

// Handler returns the agent's tunneled HTTP API.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", a.health)
	mux.HandleFunc("PUT /v1/storage", a.putStorage)
	mux.HandleFunc("POST /v1/boxes", a.createBox)
	mux.HandleFunc("POST /v1/boxes/{id}/stop", validateBoxID(a.stopBox))
	mux.HandleFunc("GET /v1/boxes/{id}/stats", validateBoxID(a.boxStats))
	mux.HandleFunc("POST /v1/boxes/{id}/start", validateBoxID(a.startBox))
	mux.HandleFunc("DELETE /v1/boxes/{id}", validateBoxID(a.deleteBox))
	mux.HandleFunc("POST /v1/boxes/{id}/exec", validateBoxID(a.execBox))
	mux.HandleFunc("GET /v1/boxes/{id}/exec", validateBoxID(a.listExec))
	mux.HandleFunc("GET /v1/boxes/{id}/exec/{processId}", validateBoxID(a.getExec))
	mux.HandleFunc("GET /v1/boxes/{id}/exec/{processId}/logs", validateBoxID(a.execLogs))
	mux.HandleFunc("POST /v1/boxes/{id}/exec/{processId}/kill", validateBoxID(a.killExec))
	mux.HandleFunc("POST /v1/boxes/{id}/exec/{processId}/stdin", validateBoxID(a.execStdin))
	mux.HandleFunc("GET /v1/boxes/{id}/files", validateBoxID(a.getFile))
	mux.HandleFunc("PUT /v1/boxes/{id}/files", validateBoxID(a.putFile))
	mux.HandleFunc("PUT /v1/boxes/{id}/archive", validateBoxID(a.putArchive))
	mux.HandleFunc("GET /v1/boxes/{id}/artifacts", validateBoxID(a.getArtifacts))
	mux.HandleFunc("POST /v1/boxes/{id}/snapshot", validateBoxID(a.captureSnapshot))
	mux.HandleFunc("GET /v1/snapshots/{sid}/tree", a.snapshotTree)
	mux.HandleFunc("GET /v1/snapshots/{sid}/file", a.snapshotFile)
	mux.HandleFunc("GET /v1/snapshots/{sid}/download", a.snapshotDownload)
	mux.HandleFunc("DELETE /v1/snapshots/{sid}", a.forgetSnapshot)
	mux.HandleFunc("POST /v1/snapshots/forget", a.forgetSnapshots)
	mux.HandleFunc("POST /v1/snapshots/prune", a.pruneSnapshots)
	mux.HandleFunc("PUT /v1/boxes/{id}/routes", validateBoxID(a.putRoutes))
	mux.HandleFunc("PUT /v1/boxes/{id}/network", validateBoxID(a.putNetwork))
	mux.HandleFunc("GET /v1/boxes/{id}/tcp/{port}", validateBoxID(a.tcpBridge))
	mux.HandleFunc("PUT /v1/drives/{name}", validateDriveName(a.putDrive))
	mux.HandleFunc("DELETE /v1/drives/{name}", validateDriveName(a.deleteDrive))
	mux.HandleFunc("POST /v1/drives/{name}/fork", validateDriveName(a.forkDrive))
	mux.HandleFunc("/v1/boxes/{id}/boxd/{path...}", validateBoxID(a.proxyBoxd))
	return logFailures(mux)
}

func (a *Agent) getArtifacts(w http.ResponseWriter, r *http.Request) {
	filePath, err := cleanBoxPath(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", "path must be an absolute path without traversal")
		return
	}
	box := boxName(r.PathValue("id"))
	var info strings.Builder
	code, err := a.docker.Exec(r.Context(), box, "root", "", nil, []string{"stat", "-L", "-c", "%F", "--", filePath}, nil, &info, io.Discard)
	if err != nil || code != 0 {
		writeError(w, http.StatusNotFound, "artifact_not_found", "artifact does not exist")
		return
	}
	if strings.TrimSpace(info.String()) == "directory" {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", "attachment; filename=artifact.tar.gz")
		code, err = a.docker.Exec(r.Context(), box, "1000:1000", "", nil, []string{"tar", "czf", "-", "-C", filePath, "."}, nil, w, io.Discard)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(path.Base(filePath)))
		code, err = a.docker.Exec(r.Context(), box, "1000:1000", "", nil, []string{"cat", "--", filePath}, nil, w, io.Discard)
	}
	if err != nil || code != 0 {
		return
	}
}

func (a *Agent) boxStats(w http.ResponseWriter, r *http.Request) {
	value, err := a.docker.CPUPercent(r.Context(), boxName(r.PathValue("id")))
	if err != nil {
		writeError(w, http.StatusBadGateway, "stats_failed", "container CPU stats are unavailable")
		return
	}
	desktopConnections, err := a.docker.DesktopConnections(r.Context(), boxName(r.PathValue("id")))
	if err != nil {
		writeError(w, http.StatusBadGateway, "stats_failed", "container desktop stats are unavailable")
		return
	}
	response := map[string]any{"cpuPercent": value, "desktopConnections": desktopConnections}
	// Usage counters are best effort: a host without the expected cgroup layout must not break the CPU reading.
	if usage, err := a.readBoxUsage(r.Context(), boxName(r.PathValue("id"))); err != nil {
		// ponytail: warn once per box; a host without the cgroup layout would otherwise log on every poll.
		if _, warned := a.usageWarned.LoadOrStore(r.PathValue("id"), true); !warned {
			log.Printf("box usage box=%s error=%q", r.PathValue("id"), err)
		}
	} else {
		response["cpuSeconds"], response["rxBytes"], response["txBytes"] = usage.CPUSeconds, usage.RxBytes, usage.TxBytes
	}
	writeJSON(w, http.StatusOK, response)
}

type storageUpdate struct {
	Repository string            `json:"repository"`
	Env        map[string]string `json:"env"`
}

func (a *Agent) putStorage(w http.ResponseWriter, r *http.Request) {
	var update storageUpdate
	if err := decodeJSON(w, r, &update); err != nil || update.Repository == "" {
		writeError(w, http.StatusBadRequest, "invalid_storage", "repository and environment are required")
		return
	}
	for key, value := range update.Env {
		if key != "AWS_ACCESS_KEY_ID" && key != "AWS_SECRET_ACCESS_KEY" && key != "AWS_DEFAULT_REGION" && key != "RESTIC_PASSWORD" {
			writeError(w, http.StatusBadRequest, "invalid_storage", "storage environment contains an unsupported variable")
			return
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			writeError(w, http.StatusBadRequest, "invalid_storage", "storage environment contains an invalid value")
			return
		}
	}
	password := update.Env["RESTIC_PASSWORD"]
	if password == "" {
		writeError(w, http.StatusBadRequest, "invalid_storage", "RESTIC_PASSWORD is required")
		return
	}
	// The engine is swapped under snapshotExclusiveMu below, so read its fields under the same lock.
	a.snapshotExclusiveMu.RLock()
	engine := snapshot.New(a.snapshots.Docker, a.snapshots.Restic, update.Repository, password)
	a.snapshotExclusiveMu.RUnlock()
	engine.Env = awsEnvironment(update.Env)
	initLock := a.storageInitLock(update.Repository)
	initLock.Lock()
	if err := engine.Init(r.Context()); err != nil {
		initLock.Unlock()
		a.writeOperationError(w, r, http.StatusBadGateway, "storage_init_failed", "snapshot repository could not be initialized", err)
		return
	}
	initLock.Unlock()
	a.snapshotExclusiveMu.Lock()
	defer a.snapshotExclusiveMu.Unlock()
	if err := writeStorageFile(a.config.StoragePath, update); err != nil {
		writeError(w, http.StatusInternalServerError, "storage_persist_failed", "snapshot storage settings could not be saved")
		return
	}
	*a.snapshots = *engine
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) storageInitLock(repository string) *sync.Mutex {
	a.storageInitMu.Lock()
	defer a.storageInitMu.Unlock()
	if lock := a.storageInitLocks[repository]; lock != nil {
		return lock
	}
	lock := &sync.Mutex{}
	a.storageInitLocks[repository] = lock
	return lock
}

func awsEnvironment(env map[string]string) map[string]string {
	result := make(map[string]string)
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_DEFAULT_REGION"} {
		if value := env[key]; value != "" {
			result[key] = value
		}
	}
	return result
}

func writeStorageFile(filename string, update storageUpdate) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(update)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(filename), ".storage-*.json")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, filename)
}

// LoadStorage reads the agent's private persisted repository override.
func LoadStorage(filename string) (string, string, map[string]string, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return "", "", nil, err
	}
	var update storageUpdate
	if err := json.Unmarshal(data, &update); err != nil {
		return "", "", nil, err
	}
	if update.Repository == "" || update.Env["RESTIC_PASSWORD"] == "" {
		return "", "", nil, errors.New("persisted snapshot storage configuration is incomplete")
	}
	return update.Repository, update.Env["RESTIC_PASSWORD"], awsEnvironment(update.Env), nil
}

func (a *Agent) health(w http.ResponseWriter, r *http.Request) {
	containers, err := a.docker.List(r.Context(), "boxes.managed=true")
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "docker_unavailable", "cannot list managed boxes")
		return
	}
	boxes := make([]map[string]string, 0, len(containers))
	for _, container := range containers {
		boxID := strings.TrimPrefix(container.Name, "box-")
		boxes = append(boxes, map[string]string{"boxId": boxID, "containerId": container.ID, "state": container.State})
	}
	procRoot := a.config.ProcRoot
	if procRoot == "" {
		procRoot = "/proc"
	}
	memory, freeMemory, swapTotal, swapFree := hostMemoryAt(procRoot)
	pressure := memoryPressureAt(procRoot)
	response := map[string]any{
		"nodeId": a.config.NodeID, "cpus": runtime.NumCPU(), "memoryBytes": memory,
		"freeMemoryBytes": freeMemory, "memAvailableBytes": freeMemory, "swapFreeBytes": swapFree, "swapTotalBytes": swapTotal,
		"memoryPressureSomeAvg10": pressure, "diskFreeBytes": diskFreeBytes("/"),
		"runtime": a.config.Runtime, "boxes": boxes,
	}

	writeJSON(w, http.StatusOK, response)
}

func (a *Agent) createBox(w http.ResponseWriter, r *http.Request) {
	var request createRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid box request")
		return
	}
	if !boxIDPattern.MatchString(request.BoxID) || request.Image == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "a valid boxId and image are required")
		return
	}
	if request.NetworkPolicy != nil {
		if err := boxtypes.ValidateNetworkPolicy(*request.NetworkPolicy); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_network_policy", err.Error())
			return
		}
	}
	if err := boxtypes.ValidateMounts(request.Mounts); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_mounts", err.Error())
		return
	}
	if request.Type == "" {
		request.Type = "default"
	}
	if _, err := boxtypes.SizeFor(request.Type); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_type", err.Error())
		return
	}
	var filesArchive []byte
	if len(request.Files) != 0 {
		archive, err := makeFilesArchive(request.Files)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_files", err.Error())
			return
		}
		filesArchive = archive
	}
	containerName := "box-" + request.BoxID
	if _, err := a.docker.Inspect(r.Context(), containerName); err == nil {
		writeError(w, http.StatusConflict, "box_exists", "box container already exists")
		return
	}
	runtimeName := request.Runtime
	if runtimeName == "" {
		runtimeName = a.config.Runtime
	}
	if err := a.docker.EnsureImage(r.Context(), request.Image); err != nil {
		if !isImageContentCorruption(err) {
			a.writeOperationError(w, r, http.StatusBadGateway, "image_unavailable", "cannot ensure box image", err)
			return
		}
		if recoveryErr := a.recoverCorruptImageContent(r.Context(), request.Image, err); recoveryErr != nil {
			a.writeOperationError(w, r, http.StatusBadGateway, "image_unavailable", "cannot recover box image", recoveryErr)
			return
		}
	}
	toolsVolume := ""
	if image := a.toolsImage(request); image == "" || strings.EqualFold(image, "none") {
		a.warnToolsUnavailable("BOXES_TOOLS_IMAGE is not configured; skipping shared tools volume")
	} else {
		volume, available, err := a.docker.EnsureToolsVolume(r.Context(), image)
		if err != nil {
			a.writeOperationError(w, r, http.StatusBadGateway, "tools_volume_failed", "cannot prepare shared tools volume", err)
			return
		}
		if available {
			toolsVolume = volume
		} else {
			a.warnToolsUnavailable(fmt.Sprintf("tools image %q is unavailable; skipping shared tools volume", image))
		}
	}
	a.networkMu.Lock()
	dnsGateway := a.dnsGateway
	a.networkMu.Unlock()
	spec := dockercli.Spec{
		BoxID: request.BoxID, Image: request.Image, Type: request.Type, Runtime: runtimeName,
		DesktopPassword: request.DesktopPassword, BoxdToken: request.BoxdToken, Env: request.Env, ToolsVolume: toolsVolume,
		Mounts: request.Mounts, DNSServer: dnsGateway,
	}
	containerID, err := a.docker.Create(r.Context(), spec)
	if isImageContentCorruption(err) {
		containerID, err = a.recoverCorruptImageCreate(r.Context(), request.Image, spec, err)
	}
	if err != nil {
		if isContainerConflict(err) {
			writeError(w, http.StatusConflict, "box_exists", "box container already exists")
		} else {
			a.writeOperationError(w, r, http.StatusBadGateway, "box_create_failed", "cannot create box container", err)
		}
		return
	}
	removeOnFailure := true
	defer func() {
		if removeOnFailure {
			_ = a.docker.Remove(context.Background(), containerName, true)
		}
	}()
	if request.FromSnapshot != "" {
		a.snapshotExclusiveMu.RLock()
		err := a.snapshots.RestoreWithStorage(r.Context(), request.BoxID, request.FromSnapshot, request.Storage.Repository, request.Storage.Env)
		a.snapshotExclusiveMu.RUnlock()
		if err != nil {
			a.writeOperationError(w, r, http.StatusBadGateway, "snapshot_restore_failed", "cannot restore box snapshot", err)
			return
		}
	}
	if request.Fork {
		markerArchive, err := makeFilesArchive([]boxFile{{Path: "/.boxes/fork", Mode: 0o600}})
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_files", "cannot create fork restore marker")
			return
		}
		if err := a.docker.CopyIn(r.Context(), containerName, "/", bytes.NewReader(markerArchive)); err != nil {
			a.writeOperationError(w, r, http.StatusBadGateway, "file_copy_failed", "cannot write fork restore marker", err)
			return
		}
	}
	if len(request.Files) != 0 {
		if err := a.docker.CopyIn(r.Context(), containerName, "/", bytes.NewReader(filesArchive)); err != nil {
			a.writeOperationError(w, r, http.StatusBadGateway, "file_copy_failed", "cannot copy box files", err)
			return
		}
	}
	if err := a.docker.Start(r.Context(), containerName); err != nil {
		a.writeOperationError(w, r, http.StatusBadGateway, "box_start_failed", "cannot start box container", err)
		return
	}
	if err := a.claimWritableMounts(r.Context(), containerName); err != nil {
		a.writeOperationError(w, r, http.StatusBadGateway, "drive_permission_failed", "cannot make box drives writable", err)
		return
	}
	if request.NetworkPolicy != nil {
		if err := a.setNetworkPolicy(r.Context(), request.BoxID, request.NetworkPolicy); err != nil {
			a.writeOperationError(w, r, http.StatusBadGateway, "network_policy_failed", "cannot apply box network policy", err)
			return
		}
	}
	if err := a.waitForBoxd(r.Context(), containerName, boxdReadyTimeout); err != nil {
		a.writeOperationError(w, r, http.StatusServiceUnavailable, "box_not_ready", "boxd did not become ready within 180 seconds", err)
		return
	}
	removeOnFailure = false
	a.mu.Lock()
	if request.BoxdToken != "" {
		a.boxdTokens[request.BoxID] = request.BoxdToken
	}
	if request.DesktopPassword != "" {
		a.desktopPasswords[request.BoxID] = request.DesktopPassword
	}
	a.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]string{"containerId": containerID})
}

func (a *Agent) recoverCorruptImageCreate(ctx context.Context, image string, spec dockercli.Spec, cause error) (string, error) {
	if err := a.recoverCorruptImageContent(ctx, image, cause); err != nil {
		return "", err
	}
	trigger := diagnostics.Sanitize(cause.Error())
	log.Printf("box image recovery step=retry_create error=%q", trigger)
	containerID, err := a.docker.Create(ctx, spec)
	if err != nil {
		log.Printf("box image recovery step=retry_create failed error=%q triggering_error=%q", diagnostics.Sanitize(err.Error()), trigger)
	}
	return containerID, err
}

func (a *Agent) recoverCorruptImageContent(ctx context.Context, image string, cause error) error {
	trigger := diagnostics.Sanitize(cause.Error())
	steps := []struct {
		name string
		run  func() error
	}{
		{name: "remove_image", run: func() error { return a.docker.RemoveImage(ctx, image) }},
		{name: "prune_dangling_images", run: func() error { return a.docker.PruneDanglingImages(ctx) }},
		{name: "pull_image", run: func() error { return a.docker.PullImage(ctx, image) }},
	}
	for _, step := range steps {
		log.Printf("box image recovery step=%s error=%q", step.name, trigger)
		if err := step.run(); err != nil {
			sanitizedErr := diagnostics.Sanitize(err.Error())
			log.Printf("box image recovery step=%s failed error=%q triggering_error=%q", step.name, sanitizedErr, trigger)
			return fmt.Errorf("box image recovery failed during %s: %w", step.name, err)
		}
	}
	return nil
}

func isImageContentCorruption(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"descriptor is neither a manifest or index",
		"unexpected media type",
		"failed to unpack image",
		"unexpected eof",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return strings.Contains(message, "content digest") && strings.Contains(message, "not found")
}

func (a *Agent) toolsImage(request createRequest) string {
	if image := strings.TrimSpace(request.ToolsImage); image != "" {
		return image
	}
	return strings.TrimSpace(a.config.ToolsImage)
}

func (a *Agent) warnToolsUnavailable(message string) {
	a.toolsWarning.Do(func() { log.Printf("%s", message) })
}

func (a *Agent) stopBox(w http.ResponseWriter, r *http.Request) {
	a.snapshotExclusiveMu.RLock()
	snapshot, err := a.snapshots.Capture(r.Context(), r.PathValue("id"))
	a.snapshotExclusiveMu.RUnlock()
	if err != nil {
		a.writeOperationError(w, r, http.StatusConflict, "snapshot_failed", "snapshot capture failed; box remains running", err)
		return
	}
	if err := a.docker.Stop(r.Context(), boxName(r.PathValue("id")), 10*time.Second); err != nil {
		a.writeOperationError(w, r, http.StatusBadGateway, "box_stop_failed", "cannot stop box container", err)
		return
	}
	a.dropNetworkRules(r.Context(), r.PathValue("id"))
	response := map[string]any{}
	response["snapshot"] = map[string]any{
		"snapshotId": snapshot.ResticID,
		"sizeBytes":  snapshot.SizeBytes,
		"createdAt":  snapshot.CreatedAt,
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *Agent) startBox(w http.ResponseWriter, r *http.Request) {
	containerName := boxName(r.PathValue("id"))
	if err := a.docker.Start(r.Context(), containerName); err != nil {
		a.writeOperationError(w, r, http.StatusBadGateway, "box_start_failed", "cannot start box container", err)
		return
	}
	if err := a.claimWritableMounts(r.Context(), containerName); err != nil {
		_ = a.docker.Stop(context.Background(), containerName, 10*time.Second)
		a.writeOperationError(w, r, http.StatusBadGateway, "drive_permission_failed", "cannot make box drives writable", err)
		return
	}
	policy, err := a.loadNetworkPolicy(r.PathValue("id"))
	if err == nil && policy != nil {
		err = a.setNetworkPolicy(r.Context(), r.PathValue("id"), policy)
	}
	if err != nil {
		_ = a.docker.Stop(context.Background(), containerName, 10*time.Second)
		a.writeOperationError(w, r, http.StatusBadGateway, "network_policy_failed", "cannot apply box network policy", err)
		return
	}
	if err := a.waitForBoxd(r.Context(), containerName, boxdReadyTimeout); err != nil {
		a.writeOperationError(w, r, http.StatusServiceUnavailable, "box_not_ready", "boxd did not become ready within 180 seconds", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// claimWritableMounts hands each writable drive root to the box user (uid 1000). Fresh volumes are root:root 755,
// so abc cannot write to them. Only root-owned roots are changed, and non-recursively, so existing data keeps its owner.
// ponytail: fixes only the mount root; a drive whose root was already chowned to another uid is left alone.
func (a *Agent) claimWritableMounts(ctx context.Context, containerName string) error {
	container, err := a.docker.Inspect(ctx, containerName)
	if err != nil {
		return err
	}
	for _, mount := range container.Mounts {
		if mount.Type != "volume" || !mount.RW {
			continue
		}
		script := `if [ "$(stat -c %u -- "$1")" = 0 ]; then chown 1000:1000 -- "$1"; fi`
		code, err := a.docker.Exec(ctx, containerName, "root", "", nil, []string{"sh", "-c", script, "sh", mount.Destination}, nil, io.Discard, io.Discard)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("chown %s exited with code %d", mount.Destination, code)
		}
	}
	return nil
}

func (a *Agent) waitForBoxd(ctx context.Context, containerName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(boxdPollInterval)
	defer ticker.Stop()
	client := &http.Client{}

	for {
		container, err := a.docker.Inspect(ctx, containerName)
		if err == nil && container.IP != "" {
			request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(container.IP, "7070")+"/healthz", nil)
			if requestErr == nil {
				response, requestErr := client.Do(request)
				if requestErr == nil {
					_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
					_ = response.Body.Close()
					if response.StatusCode == http.StatusOK {
						return nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return errBoxNotReady
		case <-ticker.C:
		}
	}
}

func (a *Agent) deleteBox(w http.ResponseWriter, r *http.Request) {
	boxID := r.PathValue("id")
	if err := a.docker.Remove(r.Context(), boxName(boxID), true); err != nil {
		writeError(w, http.StatusBadGateway, "box_delete_failed", "cannot remove box container")
		return
	}
	a.usageWarned.Delete(boxID)
	a.dropNetworkRules(r.Context(), boxID)
	if err := a.deleteNetworkPolicyFile(boxID); err != nil {
		writeError(w, http.StatusInternalServerError, "network_policy_delete_failed", "cannot remove box network policy")
		return
	}
	for _, extension := range []string{".yml", ".json"} {
		if err := os.Remove(filepath.Join(a.config.TraefikDir, "box-"+boxID+extension)); err != nil && !errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusInternalServerError, "route_delete_failed", "cannot remove box routes")
			return
		}
	}
	a.mu.Lock()
	delete(a.boxdTokens, boxID)
	delete(a.desktopPasswords, boxID)
	for host := range a.forwardAuth {
		if a.forwardAuth[host].boxID == boxID {
			delete(a.forwardAuth, host)
		}
	}
	a.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) execBox(w http.ResponseWriter, r *http.Request) {
	var request execRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid exec request")
		return
	}
	argv, err := parseCommand(request.Cmd)
	if err != nil || len(argv) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_command", "cmd must be a non-empty argv or command string")
		return
	}
	maxTimeout, defaultTimeout := 600, 5*time.Minute
	if request.Detached {
		maxTimeout, defaultTimeout = maxDetachedTimeoutSeconds, defaultDetachedTimeout
	}
	if request.TimeoutSeconds < 0 || request.TimeoutSeconds > maxTimeout {
		writeError(w, http.StatusBadRequest, "invalid_timeout", fmt.Sprintf("timeoutSeconds must be between 0 and %d", maxTimeout))
		return
	}
	if request.Stdin && !request.Detached {
		writeError(w, http.StatusBadRequest, "invalid_request", "stdin requires a detached command")
		return
	}
	if request.Desktop && request.User != "" && request.User != "abc" {
		writeError(w, http.StatusBadRequest, "invalid_request", "desktop exec runs as the default user abc only")
		return
	}
	if request.User == "" {
		request.User = "abc"
	}
	wrapper, err := a.prepareBoxUser(r.Context(), boxName(r.PathValue("id")), &request)
	if err != nil {
		switch {
		case errors.Is(err, errInvalidExecUser):
			writeError(w, http.StatusBadRequest, "invalid_user", "user must match ^[a-z_][a-z0-9_-]{0,31}$")
		case errors.Is(err, errExecUserNotFound):
			writeError(w, http.StatusNotFound, "user_not_found", "user does not exist")
		default:
			writeError(w, http.StatusBadGateway, "exec_failed", "box user could not be resolved")
		}
		return
	}
	if request.Desktop {
		desktop, err := a.desktopEnv(r.Context(), boxName(r.PathValue("id")))
		if errors.Is(err, errDesktopNotRunning) {
			writeError(w, http.StatusConflict, "desktop_not_running", "box desktop session is not running")
			return
		}
		if err != nil {
			writeError(w, http.StatusBadGateway, "exec_failed", "box desktop session could not be inspected")
			return
		}
		// Request env wins over the session env, so --env can still override a variable.
		env := maps.Clone(desktop)
		maps.Copy(env, request.Env)
		request.Env = env
	}
	if request.Cwd == "" {
		request.Cwd = defaultExecCWD(request.User)
	}
	timeout := time.Duration(request.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if request.Detached {
		boxID := r.PathValue("id")
		processID := randomProcessID()
		process := newExecProcess(boxID, argv, timeout)
		var stdinReader *os.File
		if request.Stdin {
			// An *os.File stdin is passed straight to the child, so Wait does not block on a copy goroutine.
			reader, writer, err := os.Pipe()
			if err != nil {
				writeError(w, http.StatusInternalServerError, "exec_failed", "stdin pipe could not be created")
				return
			}
			stdinReader, process.stdin = reader, writer
		}
		a.mu.Lock()
		a.pruneProcessesLocked(boxID, time.Now().UTC())
		a.processes[processID] = process
		a.mu.Unlock()
		go a.runDetached(boxName(boxID), request, argv, wrapper, timeout, processID, process, stdinReader)
		writeJSON(w, http.StatusAccepted, map[string]string{"processId": processID})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	stdout, stderr := &cappedBuffer{limit: maxExecOutput}, &cappedBuffer{limit: maxExecOutput}
	code, err := a.docker.Exec(ctx, boxName(r.PathValue("id")), request.User, request.Cwd, request.Env, append(wrapper, argv...), nil, stdout, stderr)
	if err != nil {
		writeError(w, http.StatusBadGateway, "exec_failed", "box command could not be executed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"exitCode": code, "stdout": stdout.String(), "stderr": stderr.String()})
}

func (a *Agent) getFile(w http.ResponseWriter, r *http.Request) {
	filePath, err := cleanBoxPath(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", "path must be an absolute path without traversal")
		return
	}
	box := boxName(r.PathValue("id"))
	var content, fileInfo bytes.Buffer
	code, err := a.docker.Exec(r.Context(), box, "root", "", nil, []string{"stat", "-L", "-c", "%F", "--", filePath}, nil, &fileInfo, io.Discard)
	if err != nil || code != 0 {
		writeError(w, http.StatusNotFound, "file_not_found", "box file not found")
		return
	}
	contentType := "application/octet-stream"
	if strings.TrimSpace(fileInfo.String()) == "directory" {
		code, err = a.docker.Exec(r.Context(), box, "root", "", nil, []string{"tar", "--create", "--file", "-", "--directory", filePath, "."}, nil, &content, io.Discard)
		contentType = "application/x-tar"
	} else {
		code, err = a.docker.Exec(r.Context(), box, "root", "", nil, []string{"cat", "--", filePath}, nil, &content, io.Discard)
	}
	if err != nil || code != 0 {
		writeError(w, http.StatusNotFound, "file_not_found", "box file not found")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content.Bytes())
}

func (a *Agent) putFile(w http.ResponseWriter, r *http.Request) {
	filePath, err := cleanBoxPath(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", "path must be an absolute path without traversal")
		return
	}
	mode := r.URL.Query().Get("mode")
	var parsedMode uint64
	if mode != "" {
		parsedMode, err = strconv.ParseUint(mode, 8, 12)
		if err != nil || parsedMode > 0o777 {
			writeError(w, http.StatusBadRequest, "invalid_mode", "mode must be an octal permission")
			return
		}
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil || len(data) > maxRequestBody {
		writeError(w, http.StatusRequestEntityTooLarge, "file_too_large", "file exceeds the maximum size")
		return
	}
	box := boxName(r.PathValue("id"))
	mkdirUser := "root"
	if filePath == "/config" || strings.HasPrefix(filePath, "/config/") {
		mkdirUser = "abc"
	}
	code, err := a.docker.Exec(r.Context(), box, mkdirUser, "", nil, []string{"mkdir", "-p", "--", path.Dir(filePath)}, nil, io.Discard, io.Discard)
	if err != nil || code != 0 {
		writeError(w, http.StatusBadGateway, "file_write_failed", "cannot prepare box file path")
		return
	}
	code, err = a.docker.Exec(r.Context(), box, "root", "", nil, []string{"tee", "--", filePath}, bytes.NewReader(data), io.Discard, io.Discard)
	if err != nil || code != 0 {
		writeError(w, http.StatusBadGateway, "file_write_failed", "cannot write box file")
		return
	}
	code, err = a.docker.Exec(r.Context(), box, "root", "", nil, []string{"chown", "abc:abc", "--", filePath}, nil, io.Discard, io.Discard)
	if err != nil || code != 0 {
		writeError(w, http.StatusBadGateway, "file_owner_failed", "cannot assign box file to the default user")
		return
	}
	if mode != "" {
		code, err := a.docker.Exec(r.Context(), box, "root", "", nil, []string{"chmod", strconv.FormatUint(parsedMode, 8), "--", filePath}, nil, io.Discard, io.Discard)
		if err != nil || code != 0 {
			writeError(w, http.StatusBadGateway, "file_mode_failed", "cannot set box file mode")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) putArchive(w http.ResponseWriter, r *http.Request) {
	dir, err := cleanBoxPath(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", "path must be an absolute path without traversal")
		return
	}
	limited := &archiveBodyReader{Reader: http.MaxBytesReader(w, r.Body, maxArchiveBody)}
	box := boxName(r.PathValue("id"))
	quotedDir := "'" + strings.ReplaceAll(dir, "'", "'\\''") + "'"
	argv := []string{"sh", "-c", "mkdir -p -- " + quotedDir + " && tar -xzf - -C " + quotedDir + " --no-same-owner"}
	code, err := a.docker.Exec(r.Context(), box, "1000:1000", "", nil, argv, limited, io.Discard, io.Discard)
	var maxErr *http.MaxBytesError
	if errors.As(limited.err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "archive_too_large", "archive exceeds the maximum size")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "archive_extract_failed", "cannot extract box archive")
		return
	}
	if code != 0 {
		writeError(w, http.StatusBadRequest, "archive_extract_failed", "cannot extract box archive")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type archiveBodyReader struct {
	io.Reader
	err error
}

func (r *archiveBodyReader) Read(value []byte) (int, error) {
	n, err := r.Reader.Read(value)
	if err != nil {
		r.err = err
	}
	return n, err
}

func (a *Agent) captureSnapshot(w http.ResponseWriter, r *http.Request) {
	var request struct {
		IfChanged bool `json:"ifChanged"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&request)
	}
	boxID := r.PathValue("id")
	if request.IfChanged {
		fingerprint, err := a.snapshots.Fingerprint(r.Context(), boxID)
		if err != nil {
			a.writeOperationError(w, r, http.StatusBadGateway, "snapshot_failed", "cannot inspect box changes", err)
			return
		}
		if fingerprint != "" {
			a.mu.RLock()
			previous, exists := a.snapshotFingerprints[boxID]
			a.mu.RUnlock()
			if exists && previous == fingerprint {
				writeJSON(w, http.StatusOK, map[string]bool{"skipped": true})
				return
			}
		}
		saved, err := a.captureSnapshotResult(r, boxID)
		if err != nil {
			a.writeOperationError(w, r, http.StatusBadGateway, "snapshot_failed", "cannot capture box snapshot", err)
			return
		}
		if fingerprint != "" {
			a.mu.Lock()
			a.snapshotFingerprints[boxID] = fingerprint
			a.mu.Unlock()
		}
		writeJSON(w, http.StatusCreated, map[string]any{"snapshotId": saved.ResticID, "sizeBytes": saved.SizeBytes, "createdAt": saved.CreatedAt})
		return
	}
	saved, err := a.captureSnapshotResult(r, boxID)
	if err != nil {
		a.writeOperationError(w, r, http.StatusBadGateway, "snapshot_failed", "cannot capture box snapshot", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"snapshotId": saved.ResticID, "sizeBytes": saved.SizeBytes, "createdAt": saved.CreatedAt})
}

func (a *Agent) captureSnapshotResult(r *http.Request, boxID string) (boxtypes.Snapshot, error) {
	a.snapshotExclusiveMu.RLock()
	saved, err := a.snapshots.Capture(r.Context(), boxID)
	a.snapshotExclusiveMu.RUnlock()
	return saved, err
}

func (a *Agent) snapshotTree(w http.ResponseWriter, r *http.Request) {
	storage, valid := snapshotStorageFromHeader(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid_storage", "snapshot storage context is invalid")
		return
	}
	a.snapshotExclusiveMu.RLock()
	entries, err := a.snapshots.TreeWithStorage(r.Context(), r.PathValue("sid"), storage.Repository, storage.Env)
	a.snapshotExclusiveMu.RUnlock()
	if err != nil {
		writeError(w, http.StatusBadGateway, "snapshot_read_failed", "cannot read snapshot tree")
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (a *Agent) snapshotFile(w http.ResponseWriter, r *http.Request) {
	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "path is required")
		return
	}
	storage, valid := snapshotStorageFromHeader(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid_storage", "snapshot storage context is invalid")
		return
	}
	a.snapshotExclusiveMu.RLock()
	content, err := a.snapshots.FileWithStorage(r.Context(), r.PathValue("sid"), filePath, storage.Repository, storage.Env)
	a.snapshotExclusiveMu.RUnlock()
	if err != nil {
		writeError(w, http.StatusNotFound, "snapshot_file_not_found", "snapshot file not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if _, err := tar.NewReader(bytes.NewReader(content)).Next(); err == nil {
		w.Header().Set("Content-Type", "application/x-tar")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (a *Agent) snapshotDownload(w http.ResponseWriter, r *http.Request) {
	storage, valid := snapshotStorageFromHeader(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid_storage", "snapshot storage context is invalid")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="snapshot.tar.gz"`)
	w.Header().Add("Trailer", "X-Boxes-Snapshot-Error")
	w.WriteHeader(http.StatusOK)
	gz := gzip.NewWriter(w)
	a.snapshotExclusiveMu.RLock()
	err := a.snapshots.DumpWithStorage(r.Context(), r.PathValue("sid"), storage.Repository, storage.Env, gz)
	a.snapshotExclusiveMu.RUnlock()
	if err != nil {
		_ = gz.Close()
		w.Header().Set("X-Boxes-Snapshot-Error", "snapshot dump failed")
		return
	}
	if err := gz.Close(); err != nil {
		w.Header().Set("X-Boxes-Snapshot-Error", "snapshot compression failed")
	}
}

func (a *Agent) forgetSnapshot(w http.ResponseWriter, r *http.Request) {
	storage, valid := snapshotStorageFromHeader(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid_storage", "snapshot storage context is invalid")
		return
	}
	err := func() error {
		a.snapshotExclusiveMu.Lock()
		defer a.snapshotExclusiveMu.Unlock()
		return a.snapshots.ForgetWithStorage(r.Context(), r.PathValue("sid"), storage.Repository, storage.Env)
	}()
	if err != nil {
		log.Printf("forget restic snapshot %s: %v", r.PathValue("sid"), err)
		if isUnreachableSnapshotRepository(err) {
			writeError(w, http.StatusBadGateway, "repository_unreachable", "snapshot repository is unreachable")
			return
		}
		writeError(w, http.StatusBadGateway, "snapshot_delete_failed", "cannot forget snapshot")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) forgetSnapshots(w http.ResponseWriter, r *http.Request) {
	var request struct {
		IDs []string `json:"ids"`
	}
	if err := decodeJSON(w, r, &request); err != nil || len(request.IDs) == 0 || len(request.IDs) > 1000 {
		writeError(w, http.StatusBadRequest, "invalid_snapshot_ids", "between 1 and 1000 hexadecimal snapshot IDs are required")
		return
	}
	for _, id := range request.IDs {
		if id == "" || strings.Trim(id, "0123456789abcdefABCDEF") != "" {
			writeError(w, http.StatusBadRequest, "invalid_snapshot_ids", "between 1 and 1000 hexadecimal snapshot IDs are required")
			return
		}
	}
	storage, valid := snapshotStorageFromHeader(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid_storage", "snapshot storage context is invalid")
		return
	}
	a.snapshotExclusiveMu.Lock()
	err := a.snapshots.ForgetManyWithStorage(r.Context(), request.IDs, storage.Repository, storage.Env)
	a.snapshotExclusiveMu.Unlock()
	if err != nil {
		log.Printf("forget restic snapshots: %v", err)
		if isUnreachableSnapshotRepository(err) {
			writeError(w, http.StatusBadGateway, "repository_unreachable", "snapshot repository is unreachable")
			return
		}
		writeError(w, http.StatusBadGateway, "snapshot_delete_failed", "cannot forget snapshots")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) pruneSnapshots(w http.ResponseWriter, r *http.Request) {
	storage, valid := snapshotStorageFromHeader(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid_storage", "snapshot storage context is invalid")
		return
	}
	a.snapshotExclusiveMu.Lock()
	err := a.snapshots.PruneWithStorage(r.Context(), storage.Repository, storage.Env)
	a.snapshotExclusiveMu.Unlock()
	if err != nil {
		log.Printf("prune restic snapshots: %v", err)
		if isUnreachableSnapshotRepository(err) {
			writeError(w, http.StatusBadGateway, "repository_unreachable", "snapshot repository is unreachable")
			return
		}
		writeError(w, http.StatusBadGateway, "snapshot_delete_failed", "cannot prune snapshots")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func isUnreachableSnapshotRepository(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "repository_not_found") ||
		strings.Contains(message, "repository not found") ||
		strings.Contains(message, "wrong password or no key found") ||
		strings.Contains(message, "unable to open config file")
}

func snapshotStorageFromHeader(r *http.Request) (storageUpdate, bool) {
	encoded := r.Header.Get("X-Boxes-Snapshot-Storage")
	if encoded == "" {
		return storageUpdate{}, true
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return storageUpdate{}, false
	}
	var storage storageUpdate
	if err := json.Unmarshal(data, &storage); err != nil || storage.Repository == "" {
		return storageUpdate{}, false
	}
	return storage, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	defer func() { _ = r.Body.Close() }()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}

func parseCommand(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("cmd is required")
	}
	var argv []string
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &argv); err != nil {
			return nil, err
		}
		for _, arg := range argv {
			if strings.ContainsRune(arg, '\x00') {
				return nil, errors.New("command arguments cannot contain NUL")
			}
		}
		return argv, nil
	}
	var command string
	if err := json.Unmarshal(raw, &command); err != nil {
		return nil, err
	}
	if strings.ContainsRune(command, '\x00') {
		return nil, errors.New("command cannot contain NUL")
	}
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("cmd is required")
	}
	// A string is a shell command line; pass an array to skip the shell.
	return []string{"bash", "-lc", command}, nil
}

func makeFilesArchive(files []boxFile) ([]byte, error) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, file := range files {
		name, err := cleanBoxPath(file.Path)
		if err != nil || name == "/" {
			return nil, fmt.Errorf("invalid file path %q", file.Path)
		}
		content, err := base64.StdEncoding.DecodeString(file.Content)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 content for %q", file.Path)
		}
		mode := file.Mode
		if mode == 0 {
			mode = 0o644
		}
		if mode < 0 || mode > 0o777 {
			return nil, fmt.Errorf("invalid mode for %q", file.Path)
		}
		header := &tar.Header{Name: strings.TrimPrefix(name, "/"), Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatPAX}
		if err := writer.WriteHeader(header); err != nil {
			return nil, err
		}
		if _, err := writer.Write(content); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func cleanBoxPath(value string) (string, error) {
	if value == "" || !path.IsAbs(value) || strings.ContainsRune(value, '\x00') {
		return "", errors.New("path must be absolute")
	}
	clean := path.Clean(value)
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return "", errors.New("path traversal is not allowed")
		}
	}
	return clean, nil
}

func (a *Agent) putDrive(w http.ResponseWriter, r *http.Request) {
	if err := a.docker.CreateDriveVolume(r.Context(), r.PathValue("name")); err != nil {
		a.writeOperationError(w, r, http.StatusBadGateway, "drive_create_failed", "cannot create drive volume", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) deleteDrive(w http.ResponseWriter, r *http.Request) {
	if err := a.docker.RemoveDriveVolume(r.Context(), r.PathValue("name")); err != nil {
		a.writeOperationError(w, r, http.StatusBadGateway, "drive_delete_failed", "cannot remove drive volume", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) forkDrive(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Target string `json:"target"`
		Image  string `json:"image"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid drive fork request")
		return
	}
	if err := boxtypes.ValidateDriveName(request.Target); err != nil || request.Image == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "a valid target drive and image are required")
		return
	}
	if err := a.docker.CopyDriveVolume(r.Context(), r.PathValue("name"), request.Target, request.Image); err != nil {
		a.writeOperationError(w, r, http.StatusBadGateway, "drive_fork_failed", "cannot copy drive data", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validateDriveName(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := boxtypes.ValidateDriveName(r.PathValue("name")); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_drive", err.Error())
			return
		}
		next(w, r)
	}
}

func boxName(boxID string) string { return "box-" + boxID }

func validateBoxID(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !boxIDPattern.MatchString(r.PathValue("id")) {
			writeError(w, http.StatusBadRequest, "invalid_box_id", "invalid box ID")
			return
		}
		next(w, r)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]boxtypes.APIError{"error": {Code: code, Message: message}})
}

func (a *Agent) writeOperationError(w http.ResponseWriter, r *http.Request, status int, code, message string, err error) {
	detail := diagnostics.Sanitize(err.Error())
	secrets := []string{os.Getenv("RESTIC_PASSWORD"), os.Getenv("RESTIC_REPOSITORY"), a.snapshots.Password, a.snapshots.Repo}
	for key, value := range a.snapshots.Env {
		if strings.HasPrefix(key, "AWS_") {
			secrets = append(secrets, value)
		}
	}
	for _, secret := range secrets {
		if secret != "" {
			detail = strings.ReplaceAll(detail, secret, "[REDACTED]")
		}
	}
	if detail != "" {
		message += ": " + detail
	}
	writeError(w, status, code, message)
}

type failureRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *failureRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *failureRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *failureRecorder) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.status >= 400 && w.body.Len() < 4096 {
		remaining := 4096 - w.body.Len()
		if len(data) > remaining {
			_, _ = w.body.Write(data[:remaining])
		} else {
			_, _ = w.body.Write(data)
		}
	}
	return w.ResponseWriter.Write(data)
}

func logFailures(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &failureRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if recorder.status < 400 {
			return
		}
		var result struct {
			Error boxtypes.APIError `json:"error"`
		}
		if json.Unmarshal(recorder.body.Bytes(), &result) != nil {
			result.Error.Message = recorder.body.String()
		}
		message := diagnostics.Sanitize(result.Error.Message)
		log.Printf("agent operation failed method=%s path=%s status=%d code=%s error=%q", r.Method, r.URL.Path, recorder.status, result.Error.Code, message)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func isContainerConflict(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "already exists") || strings.Contains(message, "already in use") || strings.Contains(message, "conflict")
}

func intPointer(value int) *int { return &value }

func randomProcessID() string {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return base64.RawURLEncoding.EncodeToString(bytes[:])
}

func hostMemoryAt(procRoot string) (int64, int64, int64, int64) {
	content, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return 0, 0, 0, 0
	}
	var total, available, swapTotal, swapFree int64
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = value * 1024
		case "MemAvailable:":
			available = value * 1024
		case "SwapFree:":
			swapFree = value * 1024
		case "SwapTotal:":
			swapTotal = value * 1024
		}
	}
	return total, available, swapTotal, swapFree
}

func memoryPressureAt(procRoot string) float64 {
	content, err := os.ReadFile(filepath.Join(procRoot, "pressure", "memory"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		for _, field := range fields[1:] {
			if strings.HasPrefix(field, "avg10=") {
				n, _ := strconv.ParseFloat(strings.TrimPrefix(field, "avg10="), 64)
				return n
			}
		}
	}
	return 0
}

func diskFreeBytes(directory string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(directory, &stat); err != nil {
		return 0
	}
	return int64(stat.Bavail) * int64(stat.Bsize)
}

func (a *Agent) ensureTraefikDir() error {
	return os.MkdirAll(a.config.TraefikDir, 0o700)
}

func (a *Agent) tcpBridge(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeError(w, http.StatusBadRequest, "invalid_port", "port must be between 1 and 65535")
		return
	}
	container, err := a.docker.Inspect(r.Context(), boxName(r.PathValue("id")))
	if err != nil || container.IP == "" {
		writeError(w, http.StatusBadGateway, "box_unavailable", "box container is unavailable")
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = ws.Close(websocket.StatusNormalClosure, "") }()
	remote, err := (&net.Dialer{}).DialContext(r.Context(), "tcp", net.JoinHostPort(container.IP, strconv.Itoa(port)))
	if err != nil {
		_ = ws.Close(websocket.StatusInternalError, "box port unavailable")
		return
	}
	defer func() { _ = remote.Close() }()
	stream := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
	copyDone := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(remote, stream)
		_ = remote.Close()
		copyDone <- struct{}{}
	}()
	_, _ = io.Copy(stream, remote)
	_ = stream.Close()
	select {
	case <-copyDone:
	case <-r.Context().Done():
	}
}

func (a *Agent) proxyBoxd(w http.ResponseWriter, r *http.Request) {
	boxID := r.PathValue("id")
	token, err := a.boxdToken(r.Context(), boxID)
	if err != nil {
		http.Error(w, "box unavailable", http.StatusBadGateway)
		return
	}
	if token == "" {
		http.Error(w, "boxd unavailable", http.StatusNotFound)
		return
	}
	container, err := a.docker.Inspect(r.Context(), boxName(boxID))
	if err != nil || container.IP == "" {
		http.Error(w, "box unavailable", http.StatusBadGateway)
		return
	}
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(container.IP, "7070")}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.Out.URL.Scheme = target.Scheme
			request.Out.URL.Host = target.Host
			request.Out.Host = target.Host
			suffix := strings.TrimPrefix(r.PathValue("path"), "/")
			if suffix == "" {
				request.Out.URL.Path = "/"
			} else {
				request.Out.URL.Path = path.Clean("/" + suffix)
			}
			request.Out.Header.Set("Authorization", "Bearer "+token)
		},
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(response, "boxd unavailable", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

func (a *Agent) boxdToken(ctx context.Context, boxID string) (string, error) {
	if err := a.loadContainerCredentials(ctx, boxID); err != nil {
		return "", err
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.boxdTokens[boxID], nil
}

func (a *Agent) loadContainerCredentials(ctx context.Context, boxID string) error {
	a.mu.RLock()
	_, hasPassword := a.desktopPasswords[boxID]
	_, hasToken := a.boxdTokens[boxID]
	a.mu.RUnlock()
	if hasPassword && hasToken {
		return nil
	}
	environment, err := a.docker.InspectEnv(ctx, boxName(boxID))
	if err != nil {
		return err
	}
	a.mu.Lock()
	if _, exists := a.desktopPasswords[boxID]; !exists {
		a.desktopPasswords[boxID] = environment["PASSWORD"]
	}
	if _, exists := a.boxdTokens[boxID]; !exists {
		a.boxdTokens[boxID] = environment["BOXD_TOKEN"]
	}
	a.mu.Unlock()
	return nil
}
