package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/tony1908/boxes-oss/internal/dockercli"
	"github.com/tony1908/boxes-oss/internal/snapshot"
)

const usageContainerID = "0123456789abcdef"

func TestParseCPUStatUsageReadsUsageUsec(t *testing.T) {
	got, err := parseCPUStatUsage("usage_usec 1500000\nuser_usec 1000000\nsystem_usec 500000\n")
	if err != nil || got != 1.5 {
		t.Fatalf("cpu seconds = %v, %v; want 1.5", got, err)
	}
	if _, err := parseCPUStatUsage("user_usec 1\n"); err == nil {
		t.Fatal("cpu.stat without usage_usec must be an error")
	}
}

func TestParseNetDevBytesSumsInterfacesExceptLoopback(t *testing.T) {
	const netDev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:  900000    10    0    0    0     0          0         0   900000    10    0    0    0     0       0          0
  eth0:    1200   12    0    0    0     0          0         0      3400   34    0    0    0     0       0          0
  eth1:     300    3    0    0    0     0          0         0       500    5    0    0    0     0       0          0
`
	rx, tx, err := parseNetDevBytes(netDev)
	if err != nil || rx != 1500 || tx != 3900 {
		t.Fatalf("net/dev rx=%d tx=%d err=%v; want rx=1500 tx=3900", rx, tx, err)
	}
}

// fakeUsageFiles serves host files from a map; missing paths return os.ErrNotExist.
func fakeUsageFiles(files map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		if data, ok := files[name]; ok {
			return []byte(data), nil
		}
		return nil, os.ErrNotExist
	}
}

func usageDocker(t *testing.T) *dockercli.Docker {
	return dockercli.New(testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) > 2 && argv[1] == "inspect" && argv[2] == "--format" && argv[3] == "{{.Id}} {{.State.Pid}}" {
			_, _ = io.WriteString(stdout, usageContainerID+" 4242\n")
			return 0, nil
		}
		return 1, errors.New("unexpected Docker command")
	}})
}

func TestReadBoxUsageFallsBackToDockerCgroupPath(t *testing.T) {
	service := New(Config{}, usageDocker(t), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	service.readFile = fakeUsageFiles(map[string]string{
		"/sys/fs/cgroup/docker/" + usageContainerID + "/cpu.stat": "usage_usec 2000000\n",
		"/proc/4242/net/dev": "  eth0: 10 0 0 0 0 0 0 0 20 0 0 0 0 0 0 0\n",
	})
	got, err := service.readBoxUsage(context.Background(), "box-bx_x")
	if err != nil || got != (boxUsage{CPUSeconds: 2, RxBytes: 10, TxBytes: 20}) {
		t.Fatalf("usage = %+v, %v; want cpu 2s rx 10 tx 20", got, err)
	}
}

func TestBoxStatsReportsCumulativeUsageAndKeepsCPUPercent(t *testing.T) {
	docker := dockercli.New(testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		switch {
		case len(argv) > 1 && argv[1] == "stats":
			_, _ = io.WriteString(stdout, `{"CPUPerc":"6.75%"}`)
		case len(argv) > 1 && argv[1] == "exec":
			_, _ = io.WriteString(stdout, "  sl  local_address rem_address   st\n")
		case len(argv) > 1 && argv[1] == "inspect":
			_, _ = io.WriteString(stdout, usageContainerID+" 4242\n")
		}
		return 0, nil
	}})
	service := New(Config{}, docker, snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	service.readFile = fakeUsageFiles(map[string]string{
		"/sys/fs/cgroup/system.slice/docker-" + usageContainerID + ".scope/cpu.stat": "usage_usec 3000000\n",
		"/proc/4242/net/dev": "  eth0: 100 0 0 0 0 0 0 0 200 0 0 0 0 0 0 0\n",
	})
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/boxes/bx_abcdefghijkl/stats", nil))
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK {
		t.Fatalf("stats = %d %q (err %v)", response.Code, response.Body.String(), err)
	}
	want := map[string]any{"cpuPercent": 6.75, "desktopConnections": float64(0), "cpuSeconds": 3.0, "rxBytes": float64(100), "txBytes": float64(200)}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("stats body = %#v, want %#v", body, want)
	}
}

func TestEnsureMetadataBlockAddsOneRuleAndIsIdempotent(t *testing.T) {
	docker := dockercli.New(testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) > 3 && argv[1] == "network" && argv[2] == "inspect" && argv[3] == "boxes-net" {
			_, _ = io.WriteString(stdout, "172.18.0.0/16\n")
			return 0, nil
		}
		return 1, errors.New("unexpected Docker command")
	}})
	present := false
	var firewallCalls []string
	service := New(Config{}, docker, snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	service.firewall = testExecutor{command: func(argv []string, _ []byte, _, _ io.Writer) (int, error) {
		firewallCalls = append(firewallCalls, strings.Join(argv, " "))
		if argv[3] == "-C" {
			if present {
				return 0, nil
			}
			return 1, nil
		}
		present = true
		return 0, nil
	}}
	const rule = "-w 5 %s DOCKER-USER -s 172.18.0.0/16 -d 169.254.169.254/32 -j DROP"
	if err := service.EnsureMetadataBlock(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.EnsureMetadataBlock(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"iptables " + strings.Replace(rule, "%s", "-C", 1), "iptables " + strings.Replace(rule, "%s", "-I", 1), "iptables " + strings.Replace(rule, "%s", "-C", 1)}
	if !reflect.DeepEqual(firewallCalls, want) {
		t.Fatalf("iptables calls:\n got %q\nwant %q", firewallCalls, want)
	}
}

func TestEnsureMetadataBlockFailsWithoutSubnet(t *testing.T) {
	docker := dockercli.New(testExecutor{command: func([]string, []byte, io.Writer, io.Writer) (int, error) {
		return 1, errors.New("no such network")
	}})
	service := New(Config{}, docker, snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	service.firewall = testExecutor{command: func([]string, []byte, io.Writer, io.Writer) (int, error) {
		t.Fatal("iptables must not run without a subnet")
		return 0, nil
	}}
	if err := service.EnsureMetadataBlock(context.Background()); err == nil {
		t.Fatal("EnsureMetadataBlock without boxes-net subnet = nil, want error")
	}
}
