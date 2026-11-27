package main

import (
	"runtime/debug"
	"testing"

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
