package main

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/wolfeidau/durable/internal/remote"
)

// launcher hides whether OpenShell is driven through the CLI or the SDK.
type launcher interface {
	name() string
	create(ctx context.Context, sb *sandbox, extra []string) error
	// redial returns a fresh client for the sandbox, as a server would after losing its connection.
	redial(sb *sandbox) *remote.Client
	// upload copies a local directory so that its contents appear at dest in the sandbox.
	upload(ctx context.Context, sb *sandbox, local, dest string) error
	stop(ctx context.Context, sb *sandbox) error
	start(ctx context.Context, sb *sandbox) error
	delete(ctx context.Context, name string) error
	// canUploadStdin reports whether the launcher can stream a large tar through exec stdin.
	canUploadStdin() bool
}

type cliLauncher struct{}

func (l *cliLauncher) name() string { return "cli" }

func (l *cliLauncher) create(ctx context.Context, sb *sandbox, extra []string) error {
	port, err := freePort(ctx)
	if err != nil {
		return err
	}
	sb.port = port
	if err := remote.Create(ctx, sb.name, port, sb.token, extra...); err != nil {
		return err
	}
	sb.client, err = remote.Dial(ctx, "http://127.0.0.1:"+strconv.Itoa(port), sb.token)
	return err
}

func (l *cliLauncher) redial(sb *sandbox) *remote.Client {
	c, _ := remote.Dial(context.Background(), "http://127.0.0.1:"+strconv.Itoa(sb.port), sb.token)
	return c
}

func (l *cliLauncher) upload(ctx context.Context, sb *sandbox, local, dest string) error {
	// The CLI keeps the directory's base name under dest, so "/sandbox/<base>" maps to dest "".
	if dest == "/sandbox/testdata" {
		return remote.Upload(ctx, sb.name, local, "", true)
	}
	return remote.Upload(ctx, sb.name, local, dest, false)
}

func (l *cliLauncher) stop(ctx context.Context, sb *sandbox) error { return remote.Stop(ctx, sb.name) }
func (l *cliLauncher) start(ctx context.Context, sb *sandbox) error {
	return remote.Start(ctx, sb.name)
}
func (l *cliLauncher) delete(ctx context.Context, name string) error {
	return remote.Delete(ctx, name)
}
func (l *cliLauncher) canUploadStdin() bool { return false }

type sdkLauncher struct {
	l *remote.SDKLauncher
}

func (l *sdkLauncher) name() string { return "sdk" }

func (l *sdkLauncher) create(ctx context.Context, sb *sandbox, extra []string) error {
	if err := l.l.Create(ctx, sb.name, remote.Image, sb.token, extra); err != nil {
		if lines, lerr := l.l.Logs(ctx, sb.name); lerr == nil {
			for _, ln := range lines[max(0, len(lines)-15):] {
				fmt.Println("   log:", ln)
			}
		}
		return err
	}
	sb.client = l.l.Dial(sb.name, sb.token, remote.WorkerPort)
	return nil
}

func (l *sdkLauncher) redial(sb *sandbox) *remote.Client {
	return l.l.Dial(sb.name, sb.token, remote.WorkerPort)
}

func (l *sdkLauncher) upload(ctx context.Context, sb *sandbox, local, dest string) error {
	return l.l.Upload(ctx, sb.name, local, dest)
}

func (l *sdkLauncher) stop(ctx context.Context, sb *sandbox) error  { return l.l.Stop(ctx, sb.name) }
func (l *sdkLauncher) start(ctx context.Context, sb *sandbox) error { return l.l.Start(ctx, sb.name) }
func (l *sdkLauncher) delete(ctx context.Context, name string) error {
	return l.l.Delete(ctx, name)
}
func (l *sdkLauncher) canUploadStdin() bool { return true }

func freePort(ctx context.Context) (int, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("failed to find a free port: %w", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
