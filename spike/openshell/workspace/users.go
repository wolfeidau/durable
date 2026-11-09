package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ai-openshell/sdk-go/openshell/v1/types"

	"github.com/wolfeidau/durable/internal/remote"
)

const usersImage = "durable-worker:users"

// usersProbes try to run something as another uid, or to read the worker's environment, from the bash tool.
var usersProbes = []string{
	"id; grep -E 'NoNewPrivs|CapEff|CapBnd|Seccomp:' /proc/self/status",
	"echo worker pid=$PPID; ls -ld /proc/$PPID; (tr '\\0' '\\n' < /proc/$PPID/environ | cut -d= -f1 | grep -c DURABLE_WORKER_TOKEN) 2>&1",
	"sudo -n -u tool id 2>&1",
	"setpriv --reuid=1501 --regid=1501 --clear-groups id 2>&1",
	"/usr/local/bin/suid-env id 2>&1; ls -l /usr/local/bin/suid-env",
	"su tool -c id 2>&1 </dev/null | head -3",
	"mount | grep ' /proc ' ; mount -o remount,hidepid=2 /proc 2>&1",
	"unshare -U -r id 2>&1 | head -3",
}

func runUsers(ctx context.Context) error {
	l, err := remote.NewSDKLauncher(ctx)
	if err != nil {
		return err
	}
	defer l.Close()

	var names []string
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		for _, n := range names {
			if derr := l.Delete(cctx, n); derr != nil {
				fmt.Fprintln(os.Stderr, "cleanup:", derr)
			}
		}
	}()

	step("1. build the users image")
	if err := remote.BuildImageFile(ctx, usersImage, "spike/openshell/Dockerfile.users"); err != nil {
		return err
	}

	for _, c := range []struct {
		title  string
		policy *types.SandboxPolicy
		extra  []string
	}{
		{"2. default policy: can bash become another uid or read the worker's environment?", nil, usersProbes},
		{"3. Landlock policy without /proc: is the worker's environment unreadable?", noProcPolicy(), []string{
			"id; ls /proc 2>&1 | head -3",
			"cat /proc/$PPID/environ 2>&1 | head -c 100 | wc -c; cat /proc/$PPID/environ 2>&1 | head -c 60 | tr -c 'A-Za-z_ :./-' '?' ",
			"cat /proc/self/status 2>&1 | head -2",
			"echo still-works; ls /sandbox",
		}},
	} {
		step(c.title)
		name := "dwu-" + randHex(3)
		names = append(names, name)
		workerToken := randHex(16)
		if err := l.CreateWith(ctx, name, usersImage, workerToken, nil, remote.CreateOptions{Policy: c.policy}); err != nil {
			return err
		}
		a, err := newAgent(ctx, l, name, workerToken, "")
		if err != nil {
			return err
		}
		for _, p := range c.extra {
			_, _ = a.show(ctx, p)
		}
	}
	return nil
}

func noProcPolicy() *types.SandboxPolicy {
	return &types.SandboxPolicy{
		Filesystem: &types.FilesystemPolicy{
			ReadOnly:  []string{"/usr", "/lib", "/etc", "/bin", "/sbin", "/run/openshell-supervisor-ca"},
			ReadWrite: []string{"/sandbox", "/tmp", "/dev"},
		},
		Landlock: &types.LandlockPolicy{Compatibility: "best_effort"},
	}
}
