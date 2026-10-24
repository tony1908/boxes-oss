package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// boxUsage is cumulative host-side usage for one box container since the container was created.
type boxUsage struct {
	CPUSeconds float64
	RxBytes    uint64
	TxBytes    uint64
}

// readBoxUsage reads a container's cgroup CPU time and its network byte counters from the host.
// The file reads go through a.readFile so tests can fake the filesystem.
// ponytail: counters reset when the container is recreated (resume/migrate); the server accumulates them at each stop.
func (a *Agent) readBoxUsage(ctx context.Context, name string) (boxUsage, error) {
	id, pid, err := a.docker.ContainerIDAndPID(ctx, name)
	if err != nil {
		return boxUsage{}, err
	}
	cpuStat, err := a.readFile("/sys/fs/cgroup/system.slice/docker-" + id + ".scope/cpu.stat")
	if errors.Is(err, os.ErrNotExist) {
		cpuStat, err = a.readFile("/sys/fs/cgroup/docker/" + id + "/cpu.stat")
	}
	if err != nil {
		return boxUsage{}, fmt.Errorf("read container cpu.stat: %w", err)
	}
	cpuSeconds, err := parseCPUStatUsage(string(cpuStat))
	if err != nil {
		return boxUsage{}, err
	}
	procRoot := a.config.ProcRoot
	if procRoot == "" {
		procRoot = "/proc"
	}
	netDev, err := a.readFile(filepath.Join(procRoot, strconv.Itoa(pid), "net", "dev"))
	if err != nil {
		return boxUsage{}, fmt.Errorf("read container net/dev: %w", err)
	}
	rx, tx, err := parseNetDevBytes(string(netDev))
	if err != nil {
		return boxUsage{}, err
	}
	return boxUsage{CPUSeconds: cpuSeconds, RxBytes: rx, TxBytes: tx}, nil
}

// parseCPUStatUsage returns the usage_usec field of a cgroup v2 cpu.stat file, in seconds.
func parseCPUStatUsage(data string) (float64, error) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "usage_usec" {
			continue
		}
		usec, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("decode cpu.stat usage_usec: %w", err)
		}
		return float64(usec) / 1e6, nil
	}
	return 0, errors.New("cpu.stat has no usage_usec field")
}

// parseNetDevBytes sums received and transmitted bytes over every interface except lo, from /proc/<pid>/net/dev.
func parseNetDevBytes(data string) (rx, tx uint64, err error) {
	for _, line := range strings.Split(data, "\n") {
		name, counters, ok := strings.Cut(line, ":")
		if !ok {
			continue // the two header lines have no colon
		}
		fields := strings.Fields(counters)
		if strings.TrimSpace(name) == "lo" || len(fields) < 9 {
			continue
		}
		in, errIn := strconv.ParseUint(fields[0], 10, 64)
		out, errOut := strconv.ParseUint(fields[8], 10, 64)
		if errIn != nil || errOut != nil {
			return 0, 0, fmt.Errorf("decode net/dev counters for %s", strings.TrimSpace(name))
		}
		rx += in
		tx += out
	}
	return rx, tx, nil
}
