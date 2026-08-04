package naming

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateSession(t *testing.T) {
	for _, good := range []string{"default", "work.1", "a_b-c"} {
		if err := ValidateSession(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	for _, bad := range []string{"", "has space", "../escape", "x/y"} {
		if err := ValidateSession(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestDefaultEndpointsUsePrivateUIDDirectory(t *testing.T) {
	control, data, err := DefaultEndpoints("work")
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Dir(strings.TrimPrefix(control, "ipc://"))
	if filepath.Base(wantDir) != fmt.Sprintf("shenmux-%d", os.Geteuid()) {
		t.Fatalf("endpoint directory %q is not UID-scoped", wantDir)
	}
	for _, endpoint := range []string{control, data} {
		if !strings.HasPrefix(endpoint, "ipc://"+wantDir+string(filepath.Separator)) {
			t.Fatalf("endpoint %q is not inside %q", endpoint, wantDir)
		}
	}
	if control == data {
		t.Fatal("control and data endpoints must differ")
	}
}

func TestDefaultEndpointsStayWithinDarwinIPCPathLimit(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("libzmq's shorter Unix socket limit is specific to macOS")
	}
	control, data, err := DefaultEndpoints(strings.Repeat("x", 64))
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{control, data} {
		if len(strings.TrimPrefix(endpoint, "ipc://")) >= 100 {
			t.Fatalf("IPC endpoint is too long for macOS libzmq: %d bytes (%q)", len(endpoint), endpoint)
		}
	}
}
