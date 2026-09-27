package snapshot

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type mountInfoReader interface {
	ReadMountInfo(pid string) (string, error)
}

type procMountInfo struct {
	root string
}

func newProcMountInfo() procMountInfo {
	root := os.Getenv("BOXES_HOST_PROC")
	if root == "" {
		root = "/proc"
	}
	return procMountInfo{root: root}
}

func (p procMountInfo) ReadMountInfo(pid string) (string, error) {
	numericPID, err := strconv.Atoi(pid)
	if err != nil || numericPID <= 0 {
		return "", fmt.Errorf("invalid container PID %q", pid)
	}
	data, err := os.ReadFile(filepath.Join(p.root, pid, "mountinfo"))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type upperEntryKind uint8

const (
	upperEntryOther upperEntryKind = iota
	upperEntryFile
	upperEntryDeleted
)

type upperChanges struct {
	files   []string
	deleted []string
	opaque  []string
}

func parseOverlayUpperDir(mountInfo string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(mountInfo))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 6 || separator+3 >= len(fields) || fields[4] != "/" || fields[separator+1] != "overlay" {
			continue
		}
		for _, option := range strings.Split(fields[separator+3], ",") {
			key, value, ok := strings.Cut(option, "=")
			if key != "upperdir" || !ok {
				continue
			}
			upperDir := unescapeMountInfo(value)
			if !filepath.IsAbs(upperDir) {
				return "", fmt.Errorf("overlay upperdir is not absolute: %q", upperDir)
			}
			return filepath.Clean(upperDir), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read mountinfo: %w", err)
	}
	return "", errors.New("root overlay mount with upperdir not found")
}

func unescapeMountInfo(value string) string {
	var result strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+3 < len(value) {
			escaped := value[i+1 : i+4]
			switch escaped {
			case "040":
				result.WriteByte(' ')
			case "011":
				result.WriteByte('\t')
			case "012":
				result.WriteByte('\n')
			case "134":
				result.WriteByte('\\')
			case "054":
				result.WriteByte(',')
			default:
				result.WriteByte(value[i])
				continue
			}
			i += 3
			continue
		}
		result.WriteByte(value[i])
	}
	return result.String()
}

func classifyUpperEntry(mode fs.FileMode, rdev uint64) upperEntryKind {
	if mode&os.ModeDevice != 0 && mode&os.ModeCharDevice != 0 && rdev == 0 {
		return upperEntryDeleted
	}
	if mode.IsRegular() || mode.IsDir() || mode&os.ModeSymlink != 0 {
		return upperEntryFile
	}
	return upperEntryOther
}

func walkUpperDir(root string, getXattr func(string, string) (string, error)) (upperChanges, error) {
	return walkUpperDirWith(root, getXattr, nil)
}

func walkUpperDirWith(root string, getXattr func(string, string) (string, error), classify func(string, fs.FileMode, uint64) upperEntryKind) (upperChanges, error) {
	if getXattr == nil {
		getXattr = readXattr
	}
	if classify == nil {
		classify = func(_ string, mode fs.FileMode, rdev uint64) upperEntryKind {
			return classifyUpperEntry(mode, rdev)
		}
	}
	var changes upperChanges
	err := filepath.WalkDir(root, func(hostPath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, hostPath)
		if err != nil {
			return err
		}
		containerPath := "/"
		if relative != "." {
			containerPath = "/" + filepath.ToSlash(relative)
		}
		var rdev uint64
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			rdev = uint64(stat.Rdev)
		}
		switch classify(hostPath, info.Mode(), rdev) {
		case upperEntryDeleted:
			changes.deleted = append(changes.deleted, containerPath)
		case upperEntryFile:
			if relative != "." {
				changes.files = append(changes.files, containerPath)
			}
		}
		if info.IsDir() {
			for _, name := range []string{"trusted.overlay.opaque", "user.overlay.opaque"} {
				value, err := getXattr(hostPath, name)
				if err == nil && value == "y" {
					changes.opaque = append(changes.opaque, containerPath)
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return upperChanges{}, fmt.Errorf("walk overlay upperdir %q: %w", root, err)
	}
	sort.Strings(changes.files)
	sort.Strings(changes.deleted)
	sort.Strings(changes.opaque)
	return changes, nil
}

func readXattr(filePath, name string) (string, error) {
	buffer := make([]byte, 256)
	length, err := unix.Lgetxattr(filePath, name, buffer)
	if err != nil {
		return "", err
	}
	return string(buffer[:length]), nil
}
