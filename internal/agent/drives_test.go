package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/tony1908/boxes-oss/internal/dockercli"
	"github.com/tony1908/boxes-oss/internal/snapshot"
)

func TestDriveRoutesBuildVolumeArgv(t *testing.T) {
	var calls []callRecord
	docker := dockercli.New(testExecutor{name: "docker", calls: &calls})
	service := New(Config{}, docker, snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	handler := service.Handler()
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPut, "/v1/drives/data", nil),
		httptest.NewRequest(http.MethodDelete, "/v1/drives/data", nil),
		httptest.NewRequest(http.MethodPost, "/v1/drives/data/fork", strings.NewReader(`{"target":"copy","image":"boxes/box:dev"}`)),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("%s %s = %d %q, want 204", request.Method, request.URL.Path, response.Code, response.Body.String())
		}
	}
	want := [][]string{
		{"docker", "volume", "create", "--label", "boxes.drive=data", "boxes-drive-data"},
		{"docker", "volume", "rm", "boxes-drive-data"},
		{"docker", "volume", "create", "--label", "boxes.drive=copy", "boxes-drive-copy"},
		{"docker", "run", "--rm", "--network", "none", "--runtime", "runc", "--entrypoint", "cp",
			"-v", "boxes-drive-data:/from:ro", "-v", "boxes-drive-copy:/to", "boxes/box:dev", "-a", "/from/.", "/to/"},
	}
	var got [][]string
	for _, call := range calls {
		got = append(got, call.argv)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("docker calls:\n got %q\nwant %q", got, want)
	}
}

func TestDriveRoutesRejectInvalidNames(t *testing.T) {
	service := New(Config{}, dockercli.New(testExecutor{command: func([]string, []byte, io.Writer, io.Writer) (int, error) {
		t.Fatal("docker must not run for an invalid drive name")
		return 0, nil
	}}), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/v1/drives/Bad_Name", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("PUT invalid drive = %d, want 400", response.Code)
	}
}

func TestDriveForkFailureReturnsError(t *testing.T) {
	docker := dockercli.New(testExecutor{command: func(argv []string, _ []byte, _, _ io.Writer) (int, error) {
		if argv[1] == "run" {
			return 1, errors.New("copy failed")
		}
		return 0, nil
	}})
	service := New(Config{}, docker, snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/drives/data/fork", strings.NewReader(`{"target":"copy","image":"boxes/box:dev"}`)))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("fork failure = %d, want 502", response.Code)
	}
}

func TestClaimWritableMountsChownsOnlyWritableVolumes(t *testing.T) {
	var calls []callRecord
	docker := dockercli.New(testExecutor{name: "docker", calls: &calls, command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) > 1 && argv[1] == "inspect" {
			_, _ = io.WriteString(stdout, `[{"Id":"cid","Name":"/box-bx_drives1234567","State":{"Status":"running"},"Mounts":[`+
				`{"Type":"volume","Destination":"/mnt/data","RW":true},`+
				`{"Type":"volume","Destination":"/mnt/ref","RW":false}]}]`)
		}
		return 0, nil
	}})
	service := New(Config{}, docker, snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	if err := service.claimWritableMounts(context.Background(), "box-bx_drives1234567"); err != nil {
		t.Fatal(err)
	}
	var execs [][]string
	for _, call := range calls {
		if len(call.argv) > 1 && call.argv[1] == "exec" {
			execs = append(execs, call.argv)
		}
	}
	if len(execs) != 1 {
		t.Fatalf("exec calls = %d, want one for the writable drive only: %q", len(execs), execs)
	}
	argv := execs[0]
	if !reflect.DeepEqual(argv[2:4], []string{"--user", "root"}) || argv[len(argv)-1] != "/mnt/data" {
		t.Fatalf("exec argv = %q, want root exec targeting /mnt/data", argv)
	}
}
