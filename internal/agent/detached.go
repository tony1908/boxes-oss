package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tony1908/paddock/internal/types"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxDetachedOutputBytes    = 1 << 20 // retained tail per stream
	maxDetachedTimeoutSeconds = 86400
	defaultDetachedTimeout    = time.Hour
	maxFinishedProcessAge     = time.Hour
	maxProcessesPerBox        = 100
	maxStdinWriteBytes        = 1 << 20
	stdinWriteTimeout         = 30 * time.Second
	detachedPIDDir            = "/tmp/.paddock-exec"
	logFollowGrace            = 10 * time.Second
	logPollInterval           = 200 * time.Millisecond
)

var (
	allowedKillSignals = map[string]bool{"TERM": true, "KILL": true, "INT": true, "HUP": true, "QUIT": true, "USR1": true, "USR2": true}
	pgidPattern        = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)
)

// outputBuffer retains the last maxDetachedOutputBytes written and counts every byte ever written,
// so offsets stay valid for readers that poll.
// ponytail: compacts at 2x the window so Write stays amortized O(1); the process table is in memory and is lost on agent restart.
type outputBuffer struct {
	mu    sync.Mutex
	data  []byte
	total int64
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	b.total += int64(len(p))
	if len(b.data) > 2*maxDetachedOutputBytes {
		b.data = append([]byte(nil), b.data[len(b.data)-maxDetachedOutputBytes:]...)
	}
	return len(p), nil
}

// readFrom returns retained bytes at or after offset, the new end offset, and whether
// data between offset and the retained window was dropped.
func (b *outputBuffer) readFrom(offset int64) ([]byte, int64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	retained := int64(len(b.data))
	if retained > maxDetachedOutputBytes {
		retained = maxDetachedOutputBytes
	}
	start := b.total - retained
	truncated := false
	if offset < start {
		offset, truncated = start, true
	}
	if offset > b.total {
		offset = b.total
	}
	chunk := append([]byte(nil), b.data[len(b.data)-int(b.total-offset):]...)
	return chunk, b.total, truncated
}

// execProcess is a detached command. Output is live; done closes after the exit code is set.
type execProcess struct {
	BoxID       string
	Cmd         string
	StartedAt   time.Time
	Deadline    time.Time
	Stdout      outputBuffer
	Stderr      outputBuffer
	stdinMu     sync.Mutex
	stdin       *os.File // write end; nil unless the request set stdin
	stdinClosed bool
	done        chan struct{}
	exitCode    int
	finishedAt  time.Time
}

func newExecProcess(boxID string, argv []string, timeout time.Duration) *execProcess {
	now := time.Now().UTC()
	return &execProcess{BoxID: boxID, Cmd: types.ShellJoin(argv), StartedAt: now, Deadline: now.Add(timeout), done: make(chan struct{})}
}

func (p *execProcess) finished() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// finish records the exit code and closes stdin. Callers must not hold stdinMu.
func (p *execProcess) finish(code int) {
	p.stdinMu.Lock()
	p.closeStdinLocked()
	p.stdinMu.Unlock()
	p.exitCode, p.finishedAt = code, time.Now().UTC()
	close(p.done)
}

func (p *execProcess) closeStdinLocked() {
	if p.stdin != nil && !p.stdinClosed {
		_ = p.stdin.Close()
	}
	p.stdinClosed = true
}

// detachedArgv runs argv in its own session and records the shell PID, which is also the process group ID.
// setsid --wait keeps docker exec attached until the command exits and returns its status; without it,
// setsid forks when the process is already a group leader and the exec ends early.
// wrapper is the argv prefix that switches to the box user (nil for abc and root).
// The mkdir runs as the box user before the pid file is written.
func detachedArgv(processID string, argv, wrapper []string) []string {
	script := fmt.Sprintf(`mkdir -p %s; echo $$ > "$0"; exec "$@"`, detachedPIDDir)
	out := append([]string{}, wrapper...)
	out = append(out, "setsid", "--wait", "sh", "-c", script, detachedPIDFile(processID))
	return append(out, argv...)
}

func detachedPIDFile(processID string) string {
	return fmt.Sprintf("%s/%s.pid", detachedPIDDir, processID)
}

func (a *Agent) runDetached(container string, request execRequest, argv, wrapper []string, timeout time.Duration, processID string, process *execProcess, stdinReader *os.File) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var stdin io.Reader
	if stdinReader != nil {
		stdin = stdinReader
	}
	exitCode := 1
	if len(wrapper) > 0 {
		// The box user must be able to create its pid file here; a root-owned 755 dir would block it.
		prepCode, prepErr := a.docker.Exec(ctx, container, "root", "", nil, []string{"sh", "-c", "mkdir -p " + detachedPIDDir + " && chmod 1777 " + detachedPIDDir}, nil, io.Discard, io.Discard)
		if prepErr != nil || prepCode != 0 {
			if stdinReader != nil {
				_ = stdinReader.Close()
			}
			process.finish(exitCode)
			return
		}
	}
	code, err := a.docker.Exec(ctx, container, request.User, request.Cwd, request.Env, detachedArgv(processID, argv, wrapper), stdin, &process.Stdout, &process.Stderr)
	if stdinReader != nil {
		_ = stdinReader.Close()
	}
	if err == nil {
		exitCode = code
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// The docker CLI was killed, not the box process group; stop the group explicitly.
		killCtx, killCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _ = a.signalDetached(killCtx, container, processID, "KILL")
		killCancel()
		exitCode = 124
	}
	process.finish(exitCode)
}

// signalDetached sends signal to the process group recorded in the box pid file.
// Returns the kill exit code; a non-zero code usually means the group has already exited.
func (a *Agent) signalDetached(ctx context.Context, container, processID, signal string) (int, error) {
	var pid bytes.Buffer
	code, err := a.docker.Exec(ctx, container, "root", "", nil, []string{"cat", "--", detachedPIDFile(processID)}, nil, &pid, io.Discard)
	if err != nil || code != 0 {
		return code, err
	}
	pgid := strings.TrimSpace(pid.String())
	if !pgidPattern.MatchString(pgid) {
		return 1, errors.New("pid file does not contain a process group id")
	}
	return a.docker.Exec(ctx, container, "root", "", nil, []string{"kill", "-s", signal, "--", "-" + pgid}, nil, io.Discard, io.Discard)
}

func (a *Agent) findProcess(boxID, processID string) *execProcess {
	a.mu.RLock()
	defer a.mu.RUnlock()
	process := a.processes[processID]
	if process == nil || process.BoxID != boxID {
		return nil
	}
	return process
}

// pruneProcessesLocked drops finished processes older than maxFinishedProcessAge, then the oldest
// finished processes until the box has at most maxProcessesPerBox. Running processes are never dropped.
func (a *Agent) pruneProcessesLocked(boxID string, now time.Time) {
	type entry struct {
		id      string
		process *execProcess
	}
	var finished []entry
	count := 0
	for id, process := range a.processes {
		if process.BoxID != boxID {
			continue
		}
		count++
		if !process.finished() {
			continue
		}
		if now.Sub(process.finishedAt) > maxFinishedProcessAge {
			delete(a.processes, id)
			count--
			continue
		}
		finished = append(finished, entry{id, process})
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].process.finishedAt.Before(finished[j].process.finishedAt) })
	for _, item := range finished {
		if count <= maxProcessesPerBox {
			break
		}
		delete(a.processes, item.id)
		count--
	}
}

func (a *Agent) listExec(w http.ResponseWriter, r *http.Request) {
	boxID := r.PathValue("id")
	type summary struct {
		ProcessID string    `json:"processId"`
		Cmd       string    `json:"cmd"`
		Running   bool      `json:"running"`
		ExitCode  *int      `json:"exitCode"`
		StartedAt time.Time `json:"startedAt"`
	}
	a.mu.Lock()
	a.pruneProcessesLocked(boxID, time.Now().UTC())
	commands := make([]summary, 0)
	for id, process := range a.processes {
		if process.BoxID != boxID {
			continue
		}
		item := summary{ProcessID: id, Cmd: process.Cmd, Running: !process.finished(), StartedAt: process.StartedAt}
		if !item.Running {
			item.ExitCode = intPointer(process.exitCode)
		}
		commands = append(commands, item)
	}
	a.mu.Unlock()
	sort.Slice(commands, func(i, j int) bool { return commands[i].StartedAt.Before(commands[j].StartedAt) })
	writeJSON(w, http.StatusOK, commands)
}

func (a *Agent) getExec(w http.ResponseWriter, r *http.Request) {
	process := a.findProcess(r.PathValue("id"), r.PathValue("processId"))
	if process == nil {
		writeError(w, http.StatusNotFound, "process_not_found", "exec process not found")
		return
	}
	stdout, stdoutOffset, stdoutTruncated := process.Stdout.readFrom(0)
	stderr, stderrOffset, stderrTruncated := process.Stderr.readFrom(0)
	finished := process.finished()
	result := map[string]any{
		"running": !finished, "cmd": process.Cmd, "startedAt": process.StartedAt,
		"stdout": string(stdout), "stderr": string(stderr),
		"stdoutOffset": stdoutOffset, "stderrOffset": stderrOffset,
		"stdoutTruncated": stdoutTruncated, "stderrTruncated": stderrTruncated,
	}
	if finished {
		result["exitCode"] = process.exitCode
	}
	writeJSON(w, http.StatusOK, result)
}

// execLogs streams NDJSON chunks after the given offsets. With follow=1 it keeps streaming until the process exits
// (or the command's deadline passes) and ends with {"exit":true,"exitCode":N}.
func (a *Agent) execLogs(w http.ResponseWriter, r *http.Request) {
	process := a.findProcess(r.PathValue("id"), r.PathValue("processId"))
	if process == nil {
		writeError(w, http.StatusNotFound, "process_not_found", "exec process not found")
		return
	}
	query := r.URL.Query()
	var offsets [2]int64
	for i, name := range []string{"stdoutOffset", "stderrOffset"} {
		if value := query.Get(name); value != "" {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 0 {
				writeError(w, http.StatusBadRequest, "invalid_offset", name+" must be a non-negative integer")
				return
			}
			offsets[i] = parsed
		}
	}
	follow := query.Get("follow") == "1"
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	_ = controller.Flush()
	encoder := json.NewEncoder(w)
	emit := func(stream string, buffer *outputBuffer, offset *int64) error {
		chunk, next, truncated := buffer.readFrom(*offset)
		if len(chunk) == 0 && !truncated {
			return nil
		}
		*offset = next
		line := map[string]any{"stream": stream, "data": string(chunk), "offset": next}
		if truncated {
			line["truncated"] = true
		}
		if err := encoder.Encode(line); err != nil {
			return err
		}
		return controller.Flush()
	}
	deadline := time.NewTimer(time.Until(process.Deadline) + logFollowGrace)
	defer deadline.Stop()
	ticker := time.NewTicker(logPollInterval)
	defer ticker.Stop()
	for {
		// Read the finished state before draining, so the final drain sees every byte written.
		finished := process.finished()
		if emit("stdout", &process.Stdout, &offsets[0]) != nil || emit("stderr", &process.Stderr, &offsets[1]) != nil {
			return
		}
		if finished {
			if follow {
				_ = encoder.Encode(map[string]any{"exit": true, "exitCode": process.exitCode})
				_ = controller.Flush()
			}
			return
		}
		if !follow {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return
		case <-process.done:
		case <-ticker.C:
		}
	}
}

func (a *Agent) killExec(w http.ResponseWriter, r *http.Request) {
	process := a.findProcess(r.PathValue("id"), r.PathValue("processId"))
	if process == nil {
		writeError(w, http.StatusNotFound, "process_not_found", "exec process not found")
		return
	}
	var input struct {
		Signal string `json:"signal"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil || (len(bytes.TrimSpace(body)) > 0 && json.Unmarshal(body, &input) != nil) {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid kill request")
		return
	}
	signal := strings.TrimPrefix(strings.ToUpper(input.Signal), "SIG")
	if signal == "" {
		signal = "TERM"
	}
	if !allowedKillSignals[signal] {
		writeError(w, http.StatusBadRequest, "invalid_signal", "signal must be one of TERM, KILL, INT, HUP, QUIT, USR1, USR2")
		return
	}
	if process.finished() {
		writeError(w, http.StatusConflict, "process_not_running", "exec process is not running")
		return
	}
	code, err := a.signalDetached(r.Context(), boxName(r.PathValue("id")), r.PathValue("processId"), signal)
	if err != nil {
		writeError(w, http.StatusBadGateway, "exec_failed", "box process could not be signalled")
		return
	}
	if code != 0 {
		writeError(w, http.StatusConflict, "process_not_running", "exec process is not running")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"processId": r.PathValue("processId"), "signal": signal})
}

// execStdin writes the raw request body to the process stdin; ?close=1 closes stdin afterwards.
func (a *Agent) execStdin(w http.ResponseWriter, r *http.Request) {
	process := a.findProcess(r.PathValue("id"), r.PathValue("processId"))
	if process == nil {
		writeError(w, http.StatusNotFound, "process_not_found", "exec process not found")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxStdinWriteBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "stdin_too_large", "stdin write exceeds 1 MiB")
		return
	}
	closeAfter := r.URL.Query().Get("close") == "1"
	process.stdinMu.Lock()
	defer process.stdinMu.Unlock()
	if process.stdin == nil {
		writeError(w, http.StatusConflict, "stdin_not_enabled", "exec process was not started with stdin")
		return
	}
	if process.stdinClosed {
		writeError(w, http.StatusConflict, "stdin_closed", "exec process stdin is closed")
		return
	}
	if len(body) > 0 {
		if err := process.stdin.SetWriteDeadline(time.Now().Add(stdinWriteTimeout)); err != nil {
			writeError(w, http.StatusInternalServerError, "stdin_failed", "stdin is unavailable")
			return
		}
		if _, err := process.stdin.Write(body); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				writeError(w, http.StatusGatewayTimeout, "stdin_timeout", "process did not accept stdin in time")
				return
			}
			writeError(w, http.StatusConflict, "stdin_closed", "exec process is no longer reading stdin")
			return
		}
	}
	if closeAfter {
		process.closeStdinLocked()
	}
	writeJSON(w, http.StatusOK, map[string]any{"bytes": len(body), "closed": process.stdinClosed})
}
