package commands

import (
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/hooks"
)

func newHookRunner(cfg hooks.Config, workspace, sessionDir string, yes bool) *hooks.Runner {
	mode := "default"
	if yes {
		mode = "bypassPermissions"
	}
	return hooks.New(cfg, hooks.Options{
		SessionID:      filepath.Base(sessionDir),
		Workspace:      workspace,
		PermissionMode: mode,
		LogDir:         sessionDir,
	})
}

func printHooks(out io.Writer, cfg hooks.Config, configWarnings []string, toolset []durable.Tool) {
	names := make([]string, len(toolset))
	for i, t := range toolset {
		names[i] = t.Spec().Name
	}
	for _, w := range configWarnings {
		fmt.Fprintf(out, "[hooks] warning: %s\n", w)
	}
	for _, w := range cfg.UnknownMatchers(names) {
		fmt.Fprintf(out, "[hooks] warning: %s\n", w)
	}
	counts := cfg.Counts()
	for _, event := range slices.Sorted(maps.Keys(counts)) {
		fmt.Fprintf(out, "[hooks] %s: %d configured\n", event, counts[event])
	}
}
