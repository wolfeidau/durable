// Command durable is a coding agent whose session survives crashes.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kong"
	"github.com/wolfeidau/durable/internal/commands"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// SIGINT is handled per turn by the REPL, so only SIGTERM cancels the base context.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)

	var cli commands.CLI
	k := kong.Parse(&cli,
		kong.Name("durable"),
		kong.Description("A coding agent with a crash-safe session."),
		kong.BindTo(ctx, (*context.Context)(nil)),
		kong.Bind(&commands.Globals{Logger: logger}),
	)
	err := k.Run()
	stop()
	if err != nil {
		logger.Error("command failed", slog.Any("error", err))
		os.Exit(1)
	}
}
