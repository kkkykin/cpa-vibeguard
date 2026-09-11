//go:build integration

package integration

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNativeABI(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", filepath.Join(root, "tests/integration/native_smoke.py"), filepath.Join(root, "dist/cpa-vibeguard.so"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native ABI smoke: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}
