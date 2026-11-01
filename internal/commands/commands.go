// Package commands implements the durable command line.
package commands

import (
	"log/slog"

	"github.com/alecthomas/kong"
)

// Globals is shared by every command.
type Globals struct {
	Logger *slog.Logger
}

// CLI is the root kong grammar; bare `durable` runs chat.
type CLI struct {
	Version kong.VersionFlag `help:"Print the version and exit."`

	Chat ChatCmd `cmd:"" default:"withargs" help:"Start an interactive coding agent session (default)."`
}
