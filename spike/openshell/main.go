// Command openshell is a throwaway spike that drives a tool worker inside an OpenShell sandbox.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/remote"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}
}

type sandbox struct {
	name   string
	port   int
	token  string
	client *remote.Client
}

func (s *sandbox) url() string { return fmt.Sprintf("http://127.0.0.1:%d", s.port) }

func step(title string) { fmt.Printf("\n== %s\n", title) }

func timed(label string, fn func() error) error {
	start := time.Now()
	err := fn()
	fmt.Printf("   %s: %s\n", label, time.Since(start).Round(time.Millisecond))
	return err
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func freePort(ctx context.Context) (int, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("failed to find a free port: %w", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func start(ctx context.Context, cleanup *[]string, extra ...string) (*sandbox, error) {
	port, err := freePort(ctx)
	if err != nil {
		return nil, err
	}
	s := &sandbox{name: "dspike-" + randHex(3), port: port, token: randHex(16)}
	*cleanup = append(*cleanup, s.name)
	if err := timed("create sandbox", func() error { return remote.Create(ctx, s.name, s.port, s.token, extra...) }); err != nil {
		return nil, err
	}
	if s.client, err = remote.Dial(ctx, s.url(), s.token); err != nil {
		return nil, err
	}
	if err := timed("worker ready (poll Info)", func() error { return s.client.WaitReady(ctx, 30*time.Second) }); err != nil {
		return nil, err
	}
	return s, nil
}

//nolint:gocognit // linear spike script, one block per step
func run(ctx context.Context) (err error) {
	var cleanup []string
	defer func() {
		// Runs on failure and interrupt too, so use a fresh context.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		for _, name := range cleanup {
			if derr := remote.Delete(cctx, name); derr != nil {
				fmt.Fprintln(os.Stderr, "cleanup:", derr)
			}
		}
	}()

	step("1. build image, create sandbox, dial worker")
	if err := timed("docker build", func() error { return remote.BuildImage(ctx) }); err != nil {
		return err
	}
	sb, err := start(ctx, &cleanup)
	if err != nil {
		return err
	}
	fmt.Printf("   sandbox %s forwarded at %s\n", sb.name, sb.url())

	step("2. Info")
	info, err := sb.client.Info(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("   hostname=%s uid=%d cwd=%s root=%s\n", info.Hostname, info.Uid, info.Cwd, info.Root)
	keys := make([]string, 0, len(info.Env))
	for k := range info.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("   env %s=%s\n", k, showEnv(k, info.Env[k]))
	}

	tools, err := sb.client.Tools(ctx)
	if err != nil {
		return err
	}
	byName := map[string]durable.Tool{}
	for _, t := range tools {
		ro := t.(durable.ReadOnlyTool).ReadOnly()
		fmt.Printf("   tool %-10s replay=%v read_only=%v\n", t.Spec().Name, t.Replay(), ro)
		byName[t.Spec().Name] = t
	}
	call := func(name, input string) (string, error) {
		return byName[name].Execute(ctx, durable.ToolCall{ID: "call-" + randHex(6), Name: name, Input: json.RawMessage(input)})
	}
	show := func(label string, out string, err error) {
		fmt.Printf("   %s -> err=%v\n%s\n", label, err, indent(out))
	}

	step("3. upload spike/openshell/testdata (default dest, --no-git-ignore)")
	if err := timed("upload", func() error { return remote.Upload(ctx, sb.name, "spike/openshell/testdata", "", true) }); err != nil {
		return err
	}
	out, err := call("bash", `{"command":"find /sandbox -maxdepth 3 -not -path '*/.*' | sort","timeout_seconds":null}`)
	show("find /sandbox", out, err)
	out, err = call("list_files", `{"path":null}`)
	show("list_files", out, err)

	step("3b. gitignore: upload a temp git repo with an ignored file (default flags)")
	repo, err := os.MkdirTemp("", "gitignore-demo-")
	if err != nil {
		return fmt.Errorf("failed to create temp repo: %w", err)
	}
	defer os.RemoveAll(repo)
	for name, body := range map[string]string{".gitignore": "ignored.txt\n", "kept.txt": "kept\n", "ignored.txt": "ignored\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			return fmt.Errorf("failed to write %s: %w", name, err)
		}
	}
	if gout, gerr := exec.CommandContext( //nolint:gosec // fixed arguments
		ctx, "git", "-C", repo, "init", "-q").CombinedOutput(); gerr != nil {
		return fmt.Errorf("git init failed: %w: %s", gerr, gout)
	}
	if err := remote.Upload(ctx, sb.name, repo, "/sandbox/gitdemo", false); err != nil {
		fmt.Printf("   upload failed: %v\n", err)
	}
	out, err = call("bash", `{"command":"ls -la /sandbox/gitdemo","timeout_seconds":null}`)
	show("ls gitdemo", out, err)

	step("4. tools through durable.Tool")
	out, err = call("list_files", `{"path":"."}`)
	show("list_files .", out, err)
	out, err = call("read_file", `{"path":"testdata/hello.txt","offset":null,"limit":null}`)
	if err != nil || !strings.Contains(out, "pineapple") {
		show("read_file hello.txt (unexpected)", out, err)
		out, err = call("bash", `{"command":"find /sandbox -name hello.txt","timeout_seconds":null}`)
		show("find hello.txt", out, err)
	} else {
		show("read_file hello.txt", out, err)
	}
	out, err = call("grep", `{"pattern":"pineapple","path":null,"glob":null}`)
	show("grep pineapple", out, err)
	out, err = call("edit_file", `{"path":"testdata/hello.txt","old_string":"pineapple","new_string":"mango"}`)
	show("edit_file", out, err)
	out, err = call("read_file", `{"path":"testdata/hello.txt","offset":null,"limit":null}`)
	show("read_file after edit", out, err)
	out, err = call("bash", `{"command":"uname -a; id; pwd","timeout_seconds":null}`)
	show("bash uname/id/pwd", out, err)

	step("5. egress must fail")
	out, err = call("bash", `{"command":"curl -sS -m 5 https://example.com","timeout_seconds":null}`)
	show("curl https://example.com", out, err)
	if err == nil {
		fmt.Println("   UNEXPECTED: egress succeeded")
	} else {
		fmt.Println("   egress blocked as expected")
	}

	step("5b. can the bash tool read the worker's token from its parent's environment?")
	out, err = call("bash", `{"command":"tr '\\0' '\\n' < /proc/$PPID/environ | grep -c '^DURABLE_WORKER_TOKEN=' ; cat /proc/$PPID/cmdline | tr '\\0' ' '","timeout_seconds":null}`)
	show("count of DURABLE_WORKER_TOKEN entries in worker environ (value not printed)", out, err)

	step("6. latency: 20 x bash true")
	var ds []time.Duration
	for range 20 {
		t0 := time.Now()
		if _, err := call("bash", `{"command":"true","timeout_seconds":null}`); err != nil {
			return fmt.Errorf("bash true failed: %w", err)
		}
		ds = append(ds, time.Since(t0))
	}
	slices.Sort(ds)
	fmt.Printf("   min=%s p50=%s p95=%s max=%s\n", ds[0], ds[len(ds)/2], ds[len(ds)*95/100], ds[len(ds)-1])

	step("7. recovery across a forward drop")
	if err := recovery(ctx, sb, byName["bash"]); err != nil {
		return err
	}

	step("7b. sandbox stop / start (observational)")
	stopStart(ctx, sb)

	step("8. untrusted metadata: second sandbox with --lie")
	lie, err := start(ctx, &cleanup, "--lie")
	if err != nil {
		return err
	}
	raw, err := lie.client.RawTools(ctx)
	if err != nil {
		return err
	}
	for _, r := range raw {
		fmt.Printf("   worker claims %-10s replay=%s read_only=%v\n", r.Name, r.Replay, r.ReadOnly)
	}
	ltools, err := lie.client.Tools(ctx)
	if err != nil {
		return err
	}
	for _, t := range ltools {
		fmt.Printf("   tool %-10s replay=%v read_only=%v\n", t.Spec().Name, t.Replay(), t.(durable.ReadOnlyTool).ReadOnly())
		if t.Spec().Name == "bash" && t.(durable.ReadOnlyTool).ReadOnly() {
			return errors.New("lying worker made bash read-only")
		}
		if t.Spec().Name == "exfiltrate" {
			return errors.New("exfiltrate tool was not dropped")
		}
	}
	fmt.Println("   bash ReadOnly()==false and no exfiltrate tool: lie neutralised")

	step("9. cleanup")
	cctx := context.WithoutCancel(ctx)
	for _, name := range cleanup {
		if err := remote.Delete(cctx, name); err != nil {
			return err
		}
	}
	cleanup = nil
	time.Sleep(3 * time.Second)
	sbs, fwds, err := remote.List(cctx)
	if err != nil {
		return err
	}
	fmt.Printf("   sandbox list:\n%s   forward list:\n%s", indent(sbs), indent(fwds))
	return nil
}

func recovery(ctx context.Context, sb *sandbox, bash durable.Tool) error {
	call := durable.ToolCall{ID: "recover-" + randHex(4), Name: "bash", Input: json.RawMessage(`{"command":"sleep 5; echo done","timeout_seconds":null}`)}
	errc := make(chan error, 1)
	go func() {
		_, err := bash.Execute(ctx, call)
		errc <- err
	}()
	time.Sleep(time.Second)
	if err := ForwardStopLogged(ctx, sb); err != nil {
		return err
	}
	err := <-errc
	fmt.Printf("   in-flight Execute after forward stop -> err=%v\n", err)
	time.Sleep(6 * time.Second)
	if err := remote.ForwardStart(ctx, sb.name, sb.port); err != nil {
		return err
	}
	fmt.Println("   forward restarted")
	if err := sb.client.WaitReady(ctx, 20*time.Second); err != nil {
		return err
	}
	got := bash.(durable.Recoverer).Recover(ctx, call)
	fmt.Printf("   Recover -> %q\n", got)
	if !strings.Contains(got, "done") {
		return errors.New("recovery did not return the stored output")
	}
	out, err := bash.Execute(ctx, call)
	fmt.Printf("   re-Execute with same call id -> %q err=%v (stored result, not re-run)\n", strings.TrimSpace(out), err)
	return nil
}

func stopStart(ctx context.Context, sb *sandbox) {
	report := func(label string) {
		sbs, fwds, err := remote.List(ctx)
		fmt.Printf("   [%s] err=%v\n%s%s", label, err, indent(sbs), indent(fwds))
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, perr := sb.client.Info(pctx)
		fmt.Printf("   [%s] worker Info via forward -> err=%v\n", label, perr)
	}
	if err := remote.Stop(ctx, sb.name); err != nil {
		fmt.Printf("   stop failed: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)
	report("after stop")
	if err := remote.Start(ctx, sb.name); err != nil {
		fmt.Printf("   start failed: %v\n", err)
		return
	}
	time.Sleep(3 * time.Second)
	report("after start")
	out, err := remote.Exec(ctx, sb.name, "sh", "-c", "pgrep -a durable-worker | cut -c1-80; ls /sandbox")
	fmt.Printf("   exec after start -> err=%v\n%s", err, indent(out))
	if err := remote.ForwardStart(ctx, sb.name, sb.port); err != nil {
		fmt.Printf("   forward start failed: %v\n", err)
	}
	if err := sb.client.WaitReady(ctx, 10*time.Second); err != nil {
		fmt.Printf("   worker not reachable after restart + forward: %v\n", err)
		return
	}
	fmt.Println("   worker reachable again after stop/start + forward start")
}

// ForwardStopLogged stops the forward and reports it.
func ForwardStopLogged(ctx context.Context, sb *sandbox) error {
	if err := remote.ForwardStop(ctx, sb.name, sb.port); err != nil {
		return err
	}
	fmt.Println("   forward stopped")
	return nil
}

func showEnv(k, v string) string {
	u := strings.ToUpper(k)
	if strings.Contains(u, "KEY") || strings.Contains(u, "TOKEN") || strings.Contains(u, "SECRET") {
		if strings.Contains(strings.ToLower(v), "openshell") {
			return "<redacted; looks like an OpenShell placeholder>"
		}
		return "<redacted>"
	}
	return v
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "      " + l
	}
	return strings.Join(lines, "\n") + "\n"
}
