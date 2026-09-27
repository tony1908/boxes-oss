// Package snapshot captures, restores, and inspects box filesystem snapshots.
package snapshot

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/boxes-oss/boxes/internal/diagnostics"
	boxtypes "github.com/boxes-oss/boxes/internal/types"
)

const (
	resticBinary = "restic"
	dockerBinary = "docker"
)

var excludedPaths = []string{
	"/proc", "/sys", "/dev", "/run", "/tmp", "/var/tmp",
	"/var/lib/docker", "/var/cache/apt", "/config/.cache", "/.boxes/restored",
	"/opt/boxes-tools",
	"/config/.npm/_cacache", "/config/go/pkg/mod/cache", "/config/.cargo/registry/cache",
}

const snapshotPathFilterScript = `while IFS= read -r -d '' candidate; do
	if [ -L "$candidate" ]; then
		printf '%s\0' "$candidate"
	elif [ -e "$candidate" ] && [ ! -S "$candidate" ]; then
		printf '%s\0' "$candidate"
	fi
done`

// Executor runs an external command directly, without invoking a shell.
type Executor interface {
	Run(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (exitCode int, err error)
}

type environmentExecutor interface {
	RunWithEnv(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer, env map[string]string) (exitCode int, err error)
}

// Engine performs snapshot operations using Docker and Restic executors.
type Engine struct {
	Docker    Executor
	Restic    Executor
	Repo      string
	Password  string
	Env       map[string]string
	mountInfo mountInfoReader
}

var upperDirWarning sync.Once

// Entry describes one path in a snapshot archive.
type Entry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Type string `json:"type"`
}

type boxIgnoreFile struct {
	path  string
	rules []ignoreRule
}

// New constructs a snapshot engine with the supplied external command executors.
func New(docker, restic Executor, repo, password string) *Engine {
	return &Engine{Docker: docker, Restic: restic, Repo: repo, Password: password, mountInfo: newProcMountInfo()}
}

// Capture stores a tar stream containing modified paths and deletion metadata.
func (e *Engine) Capture(ctx context.Context, boxID string) (boxtypes.Snapshot, error) {
	if err := e.validate(); err != nil {
		return boxtypes.Snapshot{}, err
	}
	if boxID == "" {
		return boxtypes.Snapshot{}, errors.New("box ID is required")
	}
	container := "box-" + boxID
	upperChanges, upperErr := e.upperDirChanges(ctx, container)
	usedDockerDiff := false
	if upperErr != nil {
		usedDockerDiff = true
		upperDirWarning.Do(func() {
			log.Printf("snapshot: unable to locate container overlay upperdir; falling back to docker diff: %v", upperErr)
		})
		output, err := e.dockerRun(ctx, []string{dockerBinary, "diff", container}, nil, nil)
		if err != nil {
			return boxtypes.Snapshot{}, err
		}
		diff, err := parseDiff(output)
		if err != nil {
			return boxtypes.Snapshot{}, err
		}
		for _, change := range diff {
			filePath := cleanAbsolutePath(change.Path)
			switch change.Kind {
			case "A", "C":
				upperChanges.files = append(upperChanges.files, filePath)
			case "D":
				upperChanges.deleted = append(upperChanges.deleted, filePath)
			}
		}
	}
	volumes, err := e.volumePaths(ctx, container)
	if err != nil {
		return boxtypes.Snapshot{}, err
	}
	ignoreFiles, err := e.boxIgnoreFiles(ctx, container)
	if err != nil {
		return boxtypes.Snapshot{}, err
	}

	var files, deleted []string
	fileSet, deletedSet := make(map[string]struct{}), make(map[string]struct{})
	for _, filePath := range upperChanges.files {
		filePath = cleanAbsolutePath(filePath)
		if filePath == "/.boxes/deletions" || filePath == "/.boxes/manifest" || filePath == "/.boxes/opaque" {
			continue
		}
		if isExcluded(filePath) || isIgnored(ignoreFiles, filePath) {
			continue
		}
		if _, exists := fileSet[filePath]; !exists {
			files = append(files, filePath)
			fileSet[filePath] = struct{}{}
		}
	}
	for _, filePath := range upperChanges.deleted {
		filePath = cleanAbsolutePath(filePath)
		if filePath == "/.boxes/deletions" || filePath == "/.boxes/manifest" || filePath == "/.boxes/opaque" {
			continue
		}
		if isExcluded(filePath) || isIgnored(ignoreFiles, filePath) {
			continue
		}
		if _, exists := deletedSet[filePath]; !exists {
			deleted = append(deleted, filePath)
			deletedSet[filePath] = struct{}{}
		}
	}
	var opaque []string
	opaqueSet := make(map[string]struct{})
	for _, filePath := range upperChanges.opaque {
		filePath = cleanAbsolutePath(filePath)
		if isExcluded(filePath) || isIgnored(ignoreFiles, filePath) {
			continue
		}
		if _, exists := opaqueSet[filePath]; !exists {
			opaque = append(opaque, filePath)
			opaqueSet[filePath] = struct{}{}
		}
	}
	for _, volume := range volumes {
		output, err := e.dockerRun(ctx, dockerExecArgs(container, false, "find", volume, "-xdev", "-print0"), nil, nil)
		if err != nil {
			return boxtypes.Snapshot{}, fmt.Errorf("enumerate volume %q: %w", volume, err)
		}
		for _, candidate := range strings.Split(output, "\x00") {
			if candidate == "" {
				continue
			}
			filePath := cleanAbsolutePath(candidate)
			if volume != "/" && filePath != volume && !strings.HasPrefix(filePath, volume+"/") {
				return boxtypes.Snapshot{}, fmt.Errorf("volume %q returned path outside mount: %q", volume, filePath)
			}
			if filePath == "/.boxes/deletions" || isExcluded(filePath) || isIgnored(ignoreFiles, filePath) {
				continue
			}
			if _, exists := fileSet[filePath]; !exists {
				files = append(files, filePath)
				fileSet[filePath] = struct{}{}
			}
		}
	}
	sort.Strings(files)
	sort.Strings(deleted)
	sort.Strings(opaque)

	if _, err := e.dockerRun(ctx, dockerExecArgs(container, false, "mkdir", "-p", "/.boxes"), nil, nil); err != nil {
		return boxtypes.Snapshot{}, fmt.Errorf("prepare snapshot metadata: %w", err)
	}
	deletionData := []byte(strings.Join(deleted, "\n"))
	if len(deleted) > 0 {
		deletionData = append(deletionData, '\n')
	}
	if _, err := e.dockerRun(ctx, dockerExecArgs(container, true, "tee", "/.boxes/deletions"), bytes.NewReader(deletionData), nil); err != nil {
		return boxtypes.Snapshot{}, fmt.Errorf("write snapshot deletion metadata: %w", err)
	}
	manifestData := nulSeparated(files)
	if _, err := e.dockerRun(ctx, dockerExecArgs(container, true, "tee", "/.boxes/manifest"), bytes.NewReader(manifestData), nil); err != nil {
		return boxtypes.Snapshot{}, fmt.Errorf("write snapshot manifest metadata: %w", err)
	}
	opaqueData := nulSeparated(opaque)
	if _, err := e.dockerRun(ctx, dockerExecArgs(container, true, "tee", "/.boxes/opaque"), bytes.NewReader(opaqueData), nil); err != nil {
		return boxtypes.Snapshot{}, fmt.Errorf("write snapshot opaque metadata: %w", err)
	}

	paths := make([]string, 0, len(files)+3)
	paths = append(paths, "/.boxes/deletions", "/.boxes/manifest", "/.boxes/opaque")
	paths = append(paths, files...)
	pathList := nulSeparated(paths)
	if usedDockerDiff {
		filteredPaths, err := e.filterSnapshotPaths(ctx, container, paths)
		if err != nil {
			return boxtypes.Snapshot{}, fmt.Errorf("filter snapshot paths: %w", err)
		}
		pathList = nulSeparated(filteredPaths)
	}

	archiveReader, archiveWriter := io.Pipe()
	tarArgs := dockerExecArgs(container, true, "nice", "-n", "19", "tar", "--create", "--ignore-failed-read", "--no-recursion", "--numeric-owner", "--xattrs", "--acls", "--file", "-", "--null", "--files-from", "-")
	tarResult := make(chan error, 1)
	go func() {
		var tarStderr bytes.Buffer
		code, runErr := e.Docker.Run(ctx, tarArgs, bytes.NewReader(pathList), archiveWriter, &tarStderr)
		if runErr != nil {
			runErr = fmt.Errorf("create snapshot tar: %s", diagnostics.Sanitize(e.redactResticSecrets(runErr.Error())))
		} else if code != 0 {
			message := diagnostics.Sanitize(e.redactResticSecrets(strings.TrimSpace(tarStderr.String())))
			if message == "" {
				runErr = fmt.Errorf("create snapshot tar failed with exit code %d", code)
			} else {
				runErr = fmt.Errorf("create snapshot tar failed with exit code %d: %s", code, message)
			}
		}
		_ = archiveWriter.CloseWithError(runErr)
		tarResult <- runErr
	}()

	var backupOutput bytes.Buffer
	backupErr := e.runRestic(ctx, archiveReader, &backupOutput, "backup", "--stdin", "--stdin-filename", "rootfs.tar", "--tag", "box="+boxID, "--json")
	_ = archiveReader.CloseWithError(backupErr)
	tarErr := <-tarResult
	if backupErr != nil {
		return boxtypes.Snapshot{}, backupErr
	}
	if tarErr != nil {
		return boxtypes.Snapshot{}, tarErr
	}
	snapshotID, size, err := parseBackupSummary(backupOutput.Bytes())
	if err != nil {
		return boxtypes.Snapshot{}, err
	}
	return boxtypes.Snapshot{ID: boxtypes.NewID("sn_"), BoxID: boxID, ResticID: snapshotID, SizeBytes: size, CreatedAt: time.Now().UTC()}, nil
}

func (e *Engine) filterSnapshotPaths(ctx context.Context, container string, paths []string) ([]string, error) {
	output, err := e.dockerRun(ctx, dockerExecArgs(container, true, "bash", "-c", snapshotPathFilterScript), bytes.NewReader(nulSeparated(paths)), nil)
	if err != nil {
		return nil, err
	}
	filtered := strings.Split(output, "\x00")
	if len(filtered) > 0 && filtered[len(filtered)-1] == "" {
		filtered = filtered[:len(filtered)-1]
	}
	return filtered, nil
}

func (e *Engine) upperDirChanges(ctx context.Context, container string) (upperChanges, error) {
	upperDir, err := e.upperDir(ctx, container)
	if err != nil {
		return upperChanges{}, err
	}
	return walkUpperDir(upperDir, nil)
}

func (e *Engine) upperDir(ctx context.Context, container string) (string, error) {
	output, err := e.dockerRun(ctx, []string{dockerBinary, "inspect", "-f", "{{.State.Pid}}", container}, nil, nil)
	if err != nil {
		return "", fmt.Errorf("inspect container PID: %w", err)
	}
	pid := strings.TrimSpace(output)
	reader := e.mountInfo
	if reader == nil {
		reader = newProcMountInfo()
	}
	mountInfo, err := reader.ReadMountInfo(pid)
	if err != nil {
		return "", fmt.Errorf("read container mountinfo: %w", err)
	}
	upperDir, err := parseOverlayUpperDir(mountInfo)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(upperDir)
	if err != nil {
		return "", fmt.Errorf("stat overlay upperdir: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("overlay upperdir %q is not a directory", upperDir)
	}
	return upperDir, nil
}

// Fingerprint identifies the current snapshot input using metadata only. An
// unavailable overlay upper directory returns an empty fingerprint so callers
// can always capture instead of incorrectly skipping.
func (e *Engine) Fingerprint(ctx context.Context, boxID string) (string, error) {
	if err := e.validate(); err != nil {
		return "", err
	}
	if boxID == "" {
		return "", errors.New("box ID is required")
	}
	container := "box-" + boxID
	upperDir, err := e.upperDir(ctx, container)
	if err != nil {
		return "", nil
	}
	entries := make([]string, 0)
	err = filepath.WalkDir(upperDir, func(hostPath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(upperDir, hostPath)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		p := "/" + filepath.ToSlash(rel)
		if isExcluded(p) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entries = append(entries, fmt.Sprintf("%s %d %d %o", p, info.Size(), info.ModTime().UnixNano(), info.Mode()))
		return nil
	})
	if err != nil {
		return "", err
	}
	volumes, err := e.volumePaths(ctx, container)
	if err != nil {
		return "", err
	}
	for _, volume := range volumes {
		output, err := e.dockerRun(ctx, dockerExecArgs(container, true, "find", volume, "-xdev", "-printf", "%p %s %T@ %m\\n"), nil, nil)
		if err != nil {
			return "", err
		}
		for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
			if line == "" {
				continue
			}
			last := strings.LastIndex(line, " ")
			if last < 0 {
				return "", fmt.Errorf("invalid volume fingerprint entry %q", line)
			}
			mode := line[last+1:]
			rest := line[:last]
			last = strings.LastIndex(rest, " ")
			if last < 0 {
				return "", fmt.Errorf("invalid volume fingerprint entry %q", line)
			}
			timestamp := rest[last+1:]
			rest = rest[:last]
			last = strings.LastIndex(rest, " ")
			if last < 0 {
				return "", fmt.Errorf("invalid volume fingerprint entry %q", line)
			}
			p := rest[:last]
			size := rest[last+1:]
			p = cleanAbsolutePath(p)
			if isExcluded(p) {
				continue
			}
			entries = append(entries, fmt.Sprintf("%s %s %s %s", p, size, timestamp, mode))
		}
	}
	sort.Strings(entries)
	digest := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return hex.EncodeToString(digest[:]), nil
}

func nulSeparated(paths []string) []byte {
	var result []byte
	for _, filePath := range paths {
		result = append(result, filePath...)
		result = append(result, 0)
	}
	return result
}

func (e *Engine) volumePaths(ctx context.Context, container string) ([]string, error) {
	output, err := e.dockerRun(ctx, []string{dockerBinary, "inspect", "--format", "{{json .Config.Volumes}}", container}, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("inspect container volumes: %w", err)
	}
	var declared map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &declared); err != nil {
		return nil, fmt.Errorf("decode container volumes: %w", err)
	}
	volumes := make([]string, 0, len(declared))
	for volume := range declared {
		volume = cleanAbsolutePath(volume)
		volumes = append(volumes, volume)
	}
	sort.Strings(volumes)
	return volumes, nil
}

// Restore streams a snapshot archive into a newly-created container.
func (e *Engine) Restore(ctx context.Context, boxID, snapshotID string) error {
	if err := e.validate(); err != nil {
		return err
	}
	if boxID == "" || snapshotID == "" {
		return errors.New("box ID and snapshot ID are required")
	}
	archiveReader, archiveWriter := io.Pipe()
	dumpResult := make(chan error, 1)
	go func() {
		err := e.runRestic(ctx, nil, archiveWriter, "dump", snapshotID, "rootfs.tar")
		_ = archiveWriter.CloseWithError(err)
		dumpResult <- err
	}()
	copyErr := e.dockerCopyIn(ctx, "box-"+boxID, "/", archiveReader)
	_ = archiveReader.CloseWithError(copyErr)
	dumpErr := <-dumpResult
	if copyErr != nil {
		return copyErr
	}
	return dumpErr
}

// RestoreWithStorage reads a snapshot from its original repository without
// changing the agent's configured repository.
func (e *Engine) RestoreWithStorage(ctx context.Context, boxID, snapshotID, repository string, env map[string]string) error {
	return e.withStorage(repository, env).Restore(ctx, boxID, snapshotID)
}

// TreeWithStorage reads an older snapshot from the repository it was captured in.
func (e *Engine) TreeWithStorage(ctx context.Context, snapshotID, repository string, env map[string]string) ([]Entry, error) {
	return e.withStorage(repository, env).Tree(ctx, snapshotID)
}

// FileWithStorage reads an older snapshot file from its original repository.
func (e *Engine) FileWithStorage(ctx context.Context, snapshotID, filePath, repository string, env map[string]string) ([]byte, error) {
	return e.withStorage(repository, env).File(ctx, snapshotID, filePath)
}

// DumpWithStorage streams the captured root filesystem tar to dst.
func (e *Engine) DumpWithStorage(ctx context.Context, snapshotID, repository string, env map[string]string, dst io.Writer) error {
	return e.withStorage(repository, env).runRestic(ctx, nil, dst, "dump", snapshotID, "rootfs.tar")
}

// ForgetWithStorage removes an older snapshot from its original repository.
func (e *Engine) ForgetWithStorage(ctx context.Context, snapshotID, repository string, env map[string]string) error {
	return e.withStorage(repository, env).Forget(ctx, snapshotID)
}

// ForgetManyWithStorage removes snapshots from their original repository without pruning data.
func (e *Engine) ForgetManyWithStorage(ctx context.Context, snapshotIDs []string, repository string, env map[string]string) error {
	return e.withStorage(repository, env).ForgetMany(ctx, snapshotIDs)
}

// PruneWithStorage removes unreferenced data from the original repository.
func (e *Engine) PruneWithStorage(ctx context.Context, repository string, env map[string]string) error {
	return e.withStorage(repository, env).Prune(ctx)
}

func (e *Engine) withStorage(repository string, env map[string]string) *Engine {
	if repository == "" {
		return e
	}
	engine := *e
	engine.Repo = repository
	engine.Password = env["RESTIC_PASSWORD"]
	engine.Env = make(map[string]string, len(env))
	for key, value := range env {
		engine.Env[key] = value
	}
	return &engine
}

// Tree returns the tar headers stored in a snapshot archive.
func (e *Engine) Tree(ctx context.Context, snapshotID string) ([]Entry, error) {
	archive, dumpResult, err := e.dumpArchive(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	reader := tar.NewReader(archive)
	var entries []Entry
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if _, err := io.Copy(io.Discard, archive); err != nil {
				_ = archive.Close()
				<-dumpResult
				return nil, fmt.Errorf("read snapshot tar trailer: %w", err)
			}
			if dumpErr := <-dumpResult; dumpErr != nil {
				return nil, dumpErr
			}
			break
		}
		if err != nil {
			_ = archive.Close()
			<-dumpResult
			return nil, fmt.Errorf("read snapshot tar: %w", err)
		}
		name := cleanTarPath(header.Name)
		if name == "/" {
			if _, err := io.Copy(io.Discard, reader); err != nil {
				_ = archive.Close()
				<-dumpResult
				return nil, fmt.Errorf("read snapshot tar entry: %w", err)
			}
			continue
		}
		entries = append(entries, Entry{Path: name, Size: header.Size, Type: tarType(header.Typeflag)})
		if _, err := io.Copy(io.Discard, reader); err != nil {
			_ = archive.Close()
			<-dumpResult
			return nil, fmt.Errorf("read snapshot tar entry: %w", err)
		}
	}
	return entries, nil
}

// File returns the bytes of one regular file stored in a snapshot archive.
func (e *Engine) File(ctx context.Context, snapshotID, filePath string) ([]byte, error) {
	if !strings.HasPrefix(filePath, "/") {
		return nil, errors.New("snapshot file path must be absolute")
	}
	wanted := cleanAbsolutePath(filePath)
	archive, dumpResult, err := e.dumpArchive(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	reader := tar.NewReader(archive)
	var content []byte
	found := false
	var directory bool
	var directoryEntries []*tar.Header
	var directoryContents [][]byte
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if _, err := io.Copy(io.Discard, archive); err != nil {
				_ = archive.Close()
				<-dumpResult
				return nil, fmt.Errorf("read snapshot tar trailer: %w", err)
			}
			break
		}
		if err != nil {
			_ = archive.Close()
			<-dumpResult
			return nil, fmt.Errorf("read snapshot tar: %w", err)
		}
		entryPath := cleanTarPath(header.Name)
		if entryPath == wanted {
			if header.Typeflag == tar.TypeDir {
				directory = true
				found = true
				directoryEntries = append(directoryEntries, tarHeaderCopy(header))
				directoryContents = append(directoryContents, nil)
				if _, err := io.Copy(io.Discard, reader); err != nil {
					_ = archive.Close()
					<-dumpResult
					return nil, err
				}
				continue
			}
			if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
				_ = archive.Close()
				<-dumpResult
				return nil, fmt.Errorf("snapshot path %q is not a regular file", wanted)
			}
			content, err = io.ReadAll(reader)
			if err != nil {
				_ = archive.Close()
				<-dumpResult
				return nil, fmt.Errorf("read snapshot file %q: %w", wanted, err)
			}
			found = true
		} else if directory && strings.HasPrefix(entryPath, strings.TrimSuffix(wanted, "/")+"/") {
			directoryEntries = append(directoryEntries, tarHeaderCopy(header))
			entry, readErr := io.ReadAll(reader)
			if readErr != nil {
				_ = archive.Close()
				<-dumpResult
				return nil, readErr
			}
			directoryContents = append(directoryContents, entry)
		} else if _, err := io.Copy(io.Discard, reader); err != nil {
			_ = archive.Close()
			<-dumpResult
			return nil, fmt.Errorf("read snapshot tar entry: %w", err)
		}
	}
	if err := <-dumpResult; err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("snapshot file %q not found", wanted)
	}
	if directory {
		var output bytes.Buffer
		writer := tar.NewWriter(&output)
		for index, header := range directoryEntries {
			header.Name = strings.TrimPrefix(strings.TrimPrefix(cleanAbsolutePath(header.Name), wanted), "/")
			if header.Name == "" {
				header.Name = "."
			}
			header.Size = int64(len(directoryContents[index]))
			if err := writer.WriteHeader(header); err != nil {
				return nil, err
			}
			if len(directoryContents[index]) != 0 {
				if _, err := writer.Write(directoryContents[index]); err != nil {
					return nil, err
				}
			}
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		return output.Bytes(), nil
	}
	return content, nil
}

func tarHeaderCopy(header *tar.Header) *tar.Header { copy := *header; return &copy }

// Forget removes a snapshot and prunes unreferenced data from the repository.
func (e *Engine) Forget(ctx context.Context, snapshotID string) error {
	if err := e.validate(); err != nil {
		return err
	}
	if snapshotID == "" {
		return errors.New("snapshot ID is required")
	}
	return e.runRestic(ctx, nil, nil, "forget", "--prune", snapshotID)
}

// ForgetMany removes snapshots without pruning repository data. IDs are sent in
// batches so a large retention backlog does not create an oversized command.
func (e *Engine) ForgetMany(ctx context.Context, snapshotIDs []string) error {
	if err := e.validate(); err != nil {
		return err
	}
	if len(snapshotIDs) == 0 {
		return errors.New("at least one snapshot ID is required")
	}
	for _, id := range snapshotIDs {
		if id == "" || strings.Trim(id, "0123456789abcdefABCDEF") != "" {
			return fmt.Errorf("invalid snapshot ID %q", id)
		}
	}
	for start := 0; start < len(snapshotIDs); start += 100 {
		end := start + 100
		if end > len(snapshotIDs) {
			end = len(snapshotIDs)
		}
		args := append([]string{"forget"}, snapshotIDs[start:end]...)
		if err := e.runRestic(ctx, nil, nil, args...); err != nil {
			return err
		}
	}
	return nil
}

// Prune removes unreferenced data from the repository.
func (e *Engine) Prune(ctx context.Context) error {
	if err := e.validate(); err != nil {
		return err
	}
	return e.runRestic(ctx, nil, nil, "prune")
}

// Init initializes the Restic repository if it is not initialized yet.
func (e *Engine) Init(ctx context.Context) error {
	if err := e.validate(); err != nil {
		return err
	}
	if err := e.runRestic(ctx, nil, nil, "cat", "config"); err == nil {
		return nil
	}
	if err := e.runRestic(ctx, nil, nil, "init"); err != nil {
		if !strings.Contains(err.Error(), "config file already exists") {
			return err
		}
		if verifyErr := e.runRestic(ctx, nil, nil, "cat", "config"); verifyErr != nil {
			return fmt.Errorf("%w; verifying existing repository config: %v", err, verifyErr)
		}
	}
	return nil
}

func (e *Engine) dumpArchive(ctx context.Context, snapshotID string) (io.ReadCloser, <-chan error, error) {
	if err := e.validate(); err != nil {
		return nil, nil, err
	}
	if snapshotID == "" {
		return nil, nil, errors.New("snapshot ID is required")
	}
	reader, writer := io.Pipe()
	result := make(chan error, 1)
	go func() {
		err := e.runRestic(ctx, nil, writer, "dump", snapshotID, "rootfs.tar")
		_ = writer.CloseWithError(err)
		result <- err
	}()
	return reader, result, nil
}

func (e *Engine) validate() error {
	if e == nil || e.Docker == nil || e.Restic == nil {
		return errors.New("docker and restic executors are required")
	}
	if e.Repo == "" {
		return errors.New("restic repository is required")
	}
	if strings.ContainsAny(e.Password, "\r\n") {
		return errors.New("restic password cannot contain a newline")
	}
	return nil
}

func (e *Engine) runRestic(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	err := e.runResticOnce(ctx, stdin, stdout, args...)
	if err == nil || len(args) == 0 || args[0] == "unlock" || !isResticLockError(err) {
		return err
	}
	// Plain unlock (no --remove-all) only removes stale locks, e.g. from a killed prune.
	if unlockErr := e.runResticOnce(ctx, nil, nil, "unlock"); unlockErr != nil {
		return fmt.Errorf("%w; restic unlock failed: %v", err, unlockErr)
	}
	// ponytail: stdin (a backup stream) is consumed and can't be replayed; the next capture retries.
	if stdin != nil {
		return err
	}
	return e.runResticOnce(ctx, stdin, stdout, args...)
}

func isResticLockError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unable to create lock") || strings.Contains(message, "already locked")
}

func (e *Engine) runResticOnce(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	passwordFile, err := os.CreateTemp("", "boxes-restic-password-")
	if err != nil {
		return fmt.Errorf("create restic password file: %w", err)
	}
	passwordPath := passwordFile.Name()
	defer os.Remove(passwordPath)
	if err := passwordFile.Chmod(0o600); err != nil {
		_ = passwordFile.Close()
		return fmt.Errorf("secure restic password file: %w", err)
	}
	if _, err := io.WriteString(passwordFile, e.Password+"\n"); err != nil {
		_ = passwordFile.Close()
		return fmt.Errorf("write restic password file: %w", err)
	}
	if err := passwordFile.Close(); err != nil {
		return fmt.Errorf("close restic password file: %w", err)
	}
	cacheDir := resticCacheDir(e.Repo)
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return fmt.Errorf("create restic cache directory: %w", err)
	}
	if err := os.Chmod(cacheDir, 0o700); err != nil {
		return fmt.Errorf("secure restic cache directory: %w", err)
	}
	argv := []string{resticBinary, "-r", e.Repo, "--password-file", passwordPath, "--cache-dir", cacheDir}
	if len(args) > 0 && isReadOnlyResticCommand(args[0]) {
		argv = append(argv, "--no-lock")
	} else {
		argv = append(argv, "--retry-lock", "5m")
	}
	argv = append(argv, args...)
	var stderr bytes.Buffer
	var code int
	var runErr error
	if executor, ok := e.Restic.(environmentExecutor); ok {
		code, runErr = executor.RunWithEnv(ctx, argv, stdin, stdout, &stderr, e.resticEnvironment())
	} else {
		code, runErr = e.Restic.Run(ctx, argv, stdin, stdout, &stderr)
	}
	if runErr == nil && code == 0 {
		return nil
	}
	message := e.redactResticSecrets(stderr.String())
	if runErr != nil {
		return fmt.Errorf("restic %s: %s", args[0], messageOrError(message, errors.New(e.redactResticSecrets(runErr.Error()))))
	}
	return fmt.Errorf("restic %s failed with exit code %d: %s", args[0], code, message)
}

func isReadOnlyResticCommand(command string) bool {
	switch command {
	case "dump", "ls", "snapshots", "cat", "stats", "find", "diff":
		return true
	default:
		return false
	}
}

func resticCacheDir(repository string) string {
	identifier := sha256.Sum256([]byte(repository))
	return filepath.Join(os.TempDir(), "boxes-restic-"+hex.EncodeToString(identifier[:8]))
}

func (e *Engine) dockerRun(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) (string, error) {
	var output, stderr bytes.Buffer
	var out io.Writer = &output
	if stdout != nil {
		out = io.MultiWriter(&output, stdout)
	}
	code, err := e.Docker.Run(ctx, argv, stdin, out, &stderr)
	if err != nil {
		return "", fmt.Errorf("docker command failed: %w", err)
	}
	if code != 0 {
		return "", fmt.Errorf("docker command failed with exit code %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return output.String(), nil
}

func (e *Engine) dockerCopyIn(ctx context.Context, container, destination string, stdin io.Reader) error {
	_, err := e.dockerRun(ctx, []string{dockerBinary, "cp", "-a", "-", container + ":" + destination}, stdin, nil)
	return err
}

func (e *Engine) boxIgnoreFiles(ctx context.Context, container string) ([]boxIgnoreFile, error) {
	output, err := e.dockerRun(ctx, dockerExecArgs(container, false, "find", "/config", "-maxdepth", "7", "-type", "f", "-name", ".boxignore", "-print"), nil, nil)
	if err != nil {
		return nil, err
	}
	paths := strings.Split(strings.TrimSpace(output), "\n")
	if len(paths) == 1 && paths[0] == "" {
		return nil, nil
	}
	sort.Strings(paths)
	var files []boxIgnoreFile
	for _, filePath := range paths {
		filePath = cleanAbsolutePath(filePath)
		if !withinConfigIgnoreDepth(filePath) {
			continue
		}
		content, err := e.dockerRun(ctx, dockerExecArgs(container, false, "cat", filePath), nil, nil)
		if err != nil {
			return nil, fmt.Errorf("read .boxignore %q: %w", filePath, err)
		}
		rules, err := parseIgnoreRules(content, filePath)
		if err != nil {
			return nil, err
		}
		files = append(files, boxIgnoreFile{path: filePath, rules: rules})
	}
	return files, nil
}

func dockerExecArgs(container string, interactive bool, argv ...string) []string {
	command := []string{dockerBinary, "exec"}
	if interactive {
		command = append(command, "--interactive")
	}
	command = append(command, "--user", "0:0", container)
	return append(command, argv...)
}

func parseDiff(output string) ([]struct{ Kind, Path string }, error) {
	var changes []struct{ Kind, Path string }
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		line = strings.TrimLeft(line, " \t")
		kind, filePath, ok := strings.Cut(line, " ")
		if !ok || filePath == "" || (kind != "A" && kind != "C" && kind != "D") {
			return nil, fmt.Errorf("invalid docker diff line %q", line)
		}
		changes = append(changes, struct{ Kind, Path string }{Kind: kind, Path: filePath})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read docker diff: %w", err)
	}
	return changes, nil
}

func isExcluded(filePath string) bool {
	for _, excluded := range excludedPaths {
		if filePath == excluded || strings.HasPrefix(filePath, excluded+"/") {
			return true
		}
	}
	parts := strings.Split(strings.TrimPrefix(filePath, "/config/"), "/")
	if strings.HasPrefix(filePath, "/config/") {
		for _, part := range parts {
			switch part {
			case "Cache", "Code Cache", "GPUCache", "CacheStorage", "ScriptCache", "DawnCache", "GrShaderCache", "ShaderCache", "cache2":
				return true
			}
		}
	}
	return false
}

func isIgnored(files []boxIgnoreFile, filePath string) bool {
	ignored := false
	for _, file := range files {
		if file.path != "/config/.boxignore" && !strings.HasPrefix(filePath, path.Dir(file.path)+"/") {
			continue
		}
		var matched bool
		ignored, matched = evaluateIgnoreRules(file.rules, filePath, ignored)
		if !matched {
			continue
		}
	}
	return ignored
}

func withinConfigIgnoreDepth(filePath string) bool {
	if !strings.HasPrefix(filePath, "/config/") {
		return false
	}
	relative := strings.TrimPrefix(filePath, "/config/")
	if path.Base(relative) != ".boxignore" {
		return false
	}
	directory := path.Dir(relative)
	if directory == "." {
		return true
	}
	return len(strings.Split(directory, "/")) <= 6
}

func cleanAbsolutePath(filePath string) string {
	return path.Clean("/" + strings.TrimPrefix(filePath, "/"))
}

func cleanTarPath(filePath string) string {
	filePath = strings.TrimPrefix(filePath, "./")
	return cleanAbsolutePath(filePath)
}

func tarType(typeFlag byte) string {
	switch typeFlag {
	case tar.TypeDir:
		return "dir"
	case tar.TypeSymlink:
		return "symlink"
	case tar.TypeLink:
		return "hardlink"
	case tar.TypeReg, tar.TypeRegA:
		return "file"
	default:
		return "special"
	}
}

func parseBackupSummary(output []byte) (string, int64, error) {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var snapshotID string
	var size int64
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var summary struct {
			SnapshotID string `json:"snapshot_id"`
			Size       int64  `json:"total_bytes_processed"`
		}
		if err := json.Unmarshal([]byte(line), &summary); err != nil {
			return "", 0, fmt.Errorf("parse restic backup JSON: %w", err)
		}
		if summary.SnapshotID != "" {
			snapshotID, size = summary.SnapshotID, summary.Size
		}
	}
	if err := scanner.Err(); err != nil {
		return "", 0, fmt.Errorf("read restic backup JSON: %w", err)
	}
	if snapshotID == "" {
		return "", 0, errors.New("restic backup did not return a snapshot ID")
	}
	return snapshotID, size, nil
}

func messageOrError(message string, err error) string {
	if message != "" {
		return message
	}
	return err.Error()
}

func (e *Engine) redactResticSecrets(message string) string {
	secrets := []string{e.Password, e.Repo}
	for key, value := range e.Env {
		if strings.HasPrefix(key, "AWS_") || key == "RESTIC_PASSWORD" {
			secrets = append(secrets, value)
		}
	}
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return strings.TrimSpace(message)
}

func (e *Engine) resticEnvironment() map[string]string {
	env := make(map[string]string)
	for key, value := range e.Env {
		if strings.HasPrefix(key, "AWS_") {
			env[key] = value
		}
	}
	return env
}
