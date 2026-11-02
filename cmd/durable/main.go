// Command durable is a coding agent whose session survives crashes.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/alecthomas/kong"

	"github.com/wolfeidau/durable/internal/commands"
)

// goreleaser sets these through -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// resolveVersion falls back to the module version, because go install sets no -ldflags.
func resolveVersion(linked string, info *debug.BuildInfo) string {
	if linked != "dev" || info == nil || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return linked
	}
	return info.Main.Version
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// SIGINT is handled per turn by the REPL, so only SIGTERM cancels the base context.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)

	info, _ := debug.ReadBuildInfo()
	var cli commands.CLI
	k := kong.Parse(&cli,
		kong.Name("durable"),
		kong.Description("A coding agent with a crash-safe session."),
		kong.BindTo(ctx, (*context.Context)(nil)),
		kong.Bind(&commands.Globals{Logger: logger}),
		kong.Vars{"version": fmt.Sprintf("%s (%s, %s)", resolveVersion(version, info), commit, date)},
	)
	err := k.Run()
	stop()
	if err != nil {
		logger.Error("command failed", slog.Any("error", err))
		os.Exit(1)
	}
}
