package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResolveVersion(t *testing.T) {
	module := func(v string) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Version: v}}
	}
	for name, tc := range map[string]struct {
		linked string
		info   *debug.BuildInfo
		want   string
	}{
		"go install":    {"dev", module("v0.1.0"), "v0.1.0"},
		"ldflags win":   {"1.2.3", module("v0.1.0"), "1.2.3"},
		"local build":   {"dev", module("(devel)"), "dev"},
		"empty version": {"dev", module(""), "dev"},
		"no build info": {"dev", nil, "dev"},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, resolveVersion(tc.linked, tc.info))
		})
	}
}

func TestVersionFlagReportsLinkedVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	bin := filepath.Join(t.TempDir(), "durable-server")
	ldflags := "-X main.version=1.2.3 -X main.commit=abc -X main.date=2026-11-01"
	out, err := exec.CommandContext(ctx, "go", "build", "-ldflags", ldflags, "-o", bin, ".").CombinedOutput()
	require.NoError(t, err, string(out))

	out, err = exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	require.NoError(t, err, string(out))
	require.Equal(t, "1.2.3 (abc, 2026-11-01)\n", string(out))
}
