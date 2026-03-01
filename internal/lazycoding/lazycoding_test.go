package lazycoding

import (
	"testing"

	"github.com/bishenghua/lazycoding/internal/config"
)

func TestCurrentDir(t *testing.T) {
	cfg := &config.Config{}
	lc := New(nil, nil, nil, cfg)

	dir := lc.currentDir("conv1")
	if dir != "" {
		t.Errorf("Expected empty dir, got %q", dir)
	}

	lc.cwdMu.Lock()
	lc.cwd["conv1"] = "/tmp/foo"
	lc.cwdMu.Unlock()

	dir = lc.currentDir("conv1")
	if dir != "/tmp/foo" {
		t.Errorf("Expected /tmp/foo, got %q", dir)
	}
}
