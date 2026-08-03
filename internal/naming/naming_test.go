package naming

import (
	"fmt"
	"os"
	"path/filepath"
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
	wantDir := filepath.Join(os.TempDir(), fmt.Sprintf("shenmux-%d", os.Geteuid()))
	for _, endpoint := range []string{control, data} {
		if !strings.HasPrefix(endpoint, "ipc://"+wantDir+string(filepath.Separator)) {
			t.Fatalf("endpoint %q is not inside %q", endpoint, wantDir)
		}
	}
	if control == data {
		t.Fatal("control and data endpoints must differ")
	}
}
