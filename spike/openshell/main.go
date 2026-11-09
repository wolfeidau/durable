// Command openshell is a throwaway spike that drives a tool worker inside an OpenShell sandbox.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/remote"
)

func main() {
	which := flag.String("launcher", "sdk", "how to drive OpenShell: sdk or cli")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, *which)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}
}

type sandbox struct {
	name   string
	port   int // CLI launcher only
	token  string
	client *remote.Client
}

type runner struct {
	l     launcher
	sb    *sandbox
	tools map[string]durable.Tool
}

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

func newLauncher(ctx context.Context, which string) (launcher, func(), error) {
	switch which {
	case "cli":
		return &cliLauncher{}, func() {}, nil
	case "sdk":
		sdk, err := remote.NewSDKLauncher(ctx)
		if err != nil {
			return nil, nil, err
		}
		wss, err := sdk.Workspaces(ctx)
		if err != nil {
			return nil, nil, err
		}
		fmt.Printf("   workspaces: %v (using %q)\n", wss, remote.Workspace)
		return &sdkLauncher{l: sdk}, func() { _ = sdk.Close() }, nil
	}
	return nil, nil, fmt.Errorf("unknown launcher %q", which)
}

func (r *runner) start(ctx context.Context, cleanup *[]string, extra ...string) (*sandbox, error) {
	sb := &sandbox{name: "dspike-" + randHex(3), token: randHex(16)}
	*cleanup = append(*cleanup, sb.name)
	if err := timed("create sandbox", func() error { return r.l.create(ctx, sb, extra) }); err != nil {
		return nil, err
	}
	if err := timed("worker ready (poll Info)", func() error { return sb.client.WaitReady(ctx, 30*time.Second) }); err != nil {
		return nil, err
	}
	return sb, nil
}

func (r *runner) loadTools(ctx context.Context) error {
	ts, err := r.sb.client.Tools(ctx)
	if err != nil {
		return err
	}
	r.tools = map[string]durable.Tool{}
	for _, t := range ts {
		r.tools[t.Spec().Name] = t
	}
	return nil
}

func (r *runner) callCtx(ctx context.Context, id, name, input string) (string, error) {
	return r.tools[name].Execute(ctx, durable.ToolCall{ID: id, Name: name, Input: json.RawMessage(input)})
}

func (r *runner) call(ctx context.Context, name, input string) (string, error) {
	return r.callCtx(ctx, "call-"+randHex(6), name, input)
}

func bashInput(cmd string) string {
	b, _ := json.Marshal(map[string]any{"command": cmd, "timeout_seconds": nil})
	return string(b)
}

func show(label string, out string, err error) {
	fmt.Printf("   %s -> err=%v\n%s\n", label, err, indent(out))
}

//nolint:gocognit // linear spike script, one block per step
func run(ctx context.Context, which string) (err error) {
	step("0. launcher " + which)
	l, closeL, err := newLauncher(ctx, which)
	if err != nil {
		return err
	}
	defer closeL()
	r := &runner{l: l}

	var cleanup []string
	defer func() {
		// Runs on failure and interrupt too, so use a fresh context.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		for _, name := range cleanup {
			if derr := l.delete(cctx, name); derr != nil {
				fmt.Fprintln(os.Stderr, "cleanup:", derr)
			}
		}
	}()

	step("1. build image, create sandbox, dial worker")
	if err := timed("docker build", func() error { return remote.BuildImage(ctx) }); err != nil {
		return err
	}
	sb, err := r.start(ctx, &cleanup)
	if err != nil {
		return err
	}
	r.sb = sb
	fmt.Printf("   sandbox %s created via %s\n", sb.name, which)

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
	if err := r.loadTools(ctx); err != nil {
		return err
	}
	for _, name := range []string{"read_file", "list_files", "grep", "edit_file", "bash"} {
		t := r.tools[name]
		fmt.Printf("   tool %-10s replay=%v read_only=%v\n", name, t.Replay(), t.(durable.ReadOnlyTool).ReadOnly())
	}

	step("3. upload spike/openshell/testdata")
	if err := timed("upload", func() error { return l.upload(ctx, sb, "spike/openshell/testdata", "/sandbox/testdata") }); err != nil {
		return err
	}
	out, err := r.call(ctx, "bash", bashInput("find /sandbox -maxdepth 3 -not -path '*/.*' | sort"))
	show("find /sandbox", out, err)
	out, err = r.call(ctx, "list_files", `{"path":null}`)
	show("list_files", out, err)

	step("3b. gitignore: upload a temp git repo with an ignored file")
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
	if err := l.upload(ctx, sb, repo, "/sandbox/gitdemo"); err != nil {
		fmt.Printf("   upload failed: %v\n", err)
	}
	out, err = r.call(ctx, "bash", bashInput("ls -la /sandbox/gitdemo"))
	show("ls gitdemo", out, err)

	step("4. tools through durable.Tool")
	out, err = r.call(ctx, "list_files", `{"path":"."}`)
	show("list_files .", out, err)
	out, err = r.call(ctx, "read_file", `{"path":"testdata/hello.txt","offset":null,"limit":null}`)
	if err != nil || !strings.Contains(out, "pineapple") {
		show("read_file hello.txt (unexpected)", out, err)
		return errors.New("read_file did not return the uploaded file")
	}
	show("read_file hello.txt", out, err)
	out, err = r.call(ctx, "grep", `{"pattern":"pineapple","path":null,"glob":null}`)
	show("grep pineapple", out, err)
	out, err = r.call(ctx, "edit_file", `{"path":"testdata/hello.txt","old_string":"pineapple","new_string":"mango"}`)
	show("edit_file", out, err)
	out, err = r.call(ctx, "read_file", `{"path":"testdata/hello.txt","offset":null,"limit":null}`)
	show("read_file after edit", out, err)
	out, err = r.call(ctx, "bash", bashInput("uname -a; id; pwd"))
	show("bash uname/id/pwd", out, err)

	step("5. egress must fail")
	out, err = r.call(ctx, "bash", bashInput("curl -sS -m 5 https://example.com"))
	show("curl https://example.com", out, err)
	if err == nil {
		fmt.Println("   UNEXPECTED: egress succeeded")
	} else {
		fmt.Println("   egress blocked as expected")
	}

	step("5b. can the bash tool read the worker's token from its parent's environment?")
	out, err = r.call(ctx, "bash", bashInput(`tr '\0' '\n' < /proc/$PPID/environ | grep -c '^DURABLE_WORKER_TOKEN=' ; cat /proc/$PPID/cmdline | tr '\0' ' '`))
	show("count of DURABLE_WORKER_TOKEN entries in worker environ (value not printed)", out, err)

	step("6. latency: 50 x bash true")
	if err := r.latency(ctx); err != nil {
		return err
	}

	step("6b. streaming: bash counts 1..5 with sleeps")
	if err := r.streaming(ctx); err != nil {
		return err
	}

	step("6c. cancel: bash sleep 30; echo late, cancelled after 1s")
	if err := r.cancel(ctx); err != nil {
		return err
	}

	if l.canUploadStdin() {
		step("6d. stdin limit: 10 MiB tar through exec stdin")
		if err := r.bigUpload(ctx); err != nil {
			return err
		}
	}

	step("7. recovery across a lost connection")
	if err := r.recovery(ctx); err != nil {
		return err
	}

	step("7b. sandbox stop / start")
	if err := r.stopStart(ctx); err != nil {
		return err
	}

	step("8. untrusted metadata: second sandbox with --lie")
	lie, err := r.start(ctx, &cleanup, "--lie")
	if err != nil {
		return err
	}
	raw, err := lie.client.RawTools(ctx)
	if err != nil {
		return err
	}
	for _, rt := range raw {
		fmt.Printf("   worker claims %-10s replay=%s read_only=%v\n", rt.Name, rt.Replay, rt.ReadOnly)
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
		if err := l.delete(cctx, name); err != nil {
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

func (r *runner) latency(ctx context.Context) error {
	var ds []time.Duration
	for range 50 {
		t0 := time.Now()
		if _, err := r.call(ctx, "bash", bashInput("true")); err != nil {
			return fmt.Errorf("bash true failed: %w", err)
		}
		ds = append(ds, time.Since(t0))
	}
	slices.Sort(ds)
	fmt.Printf("   [%s] n=50 min=%s p50=%s p95=%s max=%s\n", r.l.name(), ds[0], ds[len(ds)/2], ds[len(ds)*95/100], ds[len(ds)-1])
	return nil
}

func (r *runner) streaming(ctx context.Context) error {
	start := time.Now()
	var mu sync.Mutex
	var arrivals []time.Duration
	sctx := durable.WithToolOutputSink(ctx, func(s string) {
		mu.Lock()
		defer mu.Unlock()
		arrivals = append(arrivals, time.Since(start))
		fmt.Printf("   +%-7s delta %q\n", time.Since(start).Round(time.Millisecond), s)
	})
	out, err := r.callCtx(sctx, "stream-"+randHex(4), "bash", bashInput("for i in 1 2 3 4 5; do echo $i; sleep 0.3; done"))
	total := time.Since(start)
	fmt.Printf("   +%-7s result %q err=%v\n", total.Round(time.Millisecond), strings.TrimSpace(out), err)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	if len(arrivals) < 2 {
		return fmt.Errorf("expected several deltas, got %d", len(arrivals))
	}
	spread := arrivals[len(arrivals)-1] - arrivals[0]
	fmt.Printf("   %d deltas spread over %s (total %s)\n", len(arrivals), spread.Round(time.Millisecond), total.Round(time.Millisecond))
	if spread < time.Second {
		return errors.New("deltas were not spread over the call")
	}
	return nil
}

func (r *runner) cancel(ctx context.Context) error {
	id := "cancel-" + randHex(4)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	time.AfterFunc(time.Second, cancel)
	t0 := time.Now()
	_, err := r.callCtx(cctx, id, "bash", bashInput("sleep 30; echo late"))
	ended := time.Now()
	fmt.Printf("   Execute returned after %s, %s after cancel, err=%v\n", ended.Sub(t0).Round(time.Millisecond), ended.Sub(t0.Add(time.Second)).Round(time.Millisecond), err)
	if !errors.Is(err, context.Canceled) {
		return fmt.Errorf("expected context.Canceled, got: %w", err)
	}
	if ended.Sub(t0) > 2500*time.Millisecond {
		return errors.New("cancel took too long")
	}
	var got string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got = r.tools["bash"].(durable.Recoverer).Recover(ctx, durable.ToolCall{ID: id, Name: "bash"})
		if !strings.Contains(got, "no result") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Printf("   stored result via Recover: %q (%s after Execute returned)\n", got, time.Since(ended).Round(time.Millisecond))
	if strings.Contains(got, "late") || strings.Contains(got, "no result") {
		return errors.New("stored result missing or contains late")
	}
	again, aerr := r.callCtx(ctx, id, "bash", bashInput("sleep 30; echo late"))
	fmt.Printf("   repeat Execute, same id -> %q err=%v (stored, not re-run)\n", again, aerr)
	return nil
}

func (r *runner) bigUpload(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "big-upload-")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)
	var seed [32]byte
	buf := make([]byte, 10<<20)
	_, _ = mrand.NewChaCha8(seed).Read(buf)
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), buf, 0o600); err != nil {
		return fmt.Errorf("failed to write blob: %w", err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256(buf))
	if err := timed("upload 10 MiB tar", func() error { return r.l.upload(ctx, r.sb, dir, "/sandbox/big") }); err != nil {
		return err
	}
	out, err := r.call(ctx, "bash", bashInput("stat -c %s /sandbox/big/blob.bin; sha256sum /sandbox/big/blob.bin"))
	show("size and sha256 in sandbox", out, err)
	fmt.Printf("   local size=%d sha256=%s\n", len(buf), want)
	if err != nil || !strings.Contains(out, want) || !strings.Contains(out, "10485760") {
		return errors.New("uploaded file does not match")
	}
	fmt.Println("   match")
	return nil
}

func (r *runner) recovery(ctx context.Context) error {
	id := "recover-" + randHex(4)
	bash := r.tools["bash"]
	call := durable.ToolCall{ID: id, Name: "bash", Input: json.RawMessage(bashInput("sleep 5; echo done"))}
	errc := make(chan error, 1)
	go func() {
		_, err := bash.Execute(ctx, call)
		errc <- err
	}()
	time.Sleep(time.Second)
	if r.l.name() == "cli" {
		if err := remote.ForwardStop(ctx, r.sb.name, r.sb.port); err != nil {
			return err
		}
		fmt.Println("   forward stopped")
	} else {
		r.sb.client.CloseIdle()
		r.sb.client.DropConnections()
		fmt.Println("   all forward streams closed mid-call")
	}
	err := <-errc
	fmt.Printf("   in-flight Execute after connection loss -> err=%v\n", err)
	time.Sleep(6 * time.Second)
	if r.l.name() == "cli" {
		if err := remote.ForwardStart(ctx, r.sb.name, r.sb.port); err != nil {
			return err
		}
		fmt.Println("   forward restarted")
	}
	r.sb.client = r.l.redial(r.sb)
	if err := r.sb.client.WaitReady(ctx, 20*time.Second); err != nil {
		return err
	}
	if err := r.loadTools(ctx); err != nil {
		return err
	}
	got := r.tools["bash"].(durable.Recoverer).Recover(ctx, call)
	fmt.Printf("   Recover on a fresh dial -> %q\n", got)
	if !strings.Contains(got, "done") {
		return errors.New("recovery did not return the stored output")
	}
	out, err := r.tools["bash"].Execute(ctx, call)
	fmt.Printf("   re-Execute with same call id -> %q err=%v (stored result, not re-run)\n", strings.TrimSpace(out), err)
	return nil
}

func (r *runner) stopStart(ctx context.Context) error {
	if err := timed("stop + wait stopped", func() error { return r.l.stop(ctx, r.sb) }); err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, perr := r.sb.client.Info(pctx)
	cancel()
	fmt.Printf("   Info while stopped -> err=%v\n", perr)
	if err := timed("start + wait ready", func() error { return r.l.start(ctx, r.sb) }); err != nil {
		return err
	}
	if r.l.name() == "cli" {
		if err := remote.ForwardStart(ctx, r.sb.name, r.sb.port); err != nil {
			return err
		}
	}
	r.sb.client = r.l.redial(r.sb)
	if err := timed("worker ready after restart (new dial)", func() error { return r.sb.client.WaitReady(ctx, 30*time.Second) }); err != nil {
		return err
	}
	if err := r.loadTools(ctx); err != nil {
		return err
	}
	out, err := r.call(ctx, "bash", bashInput("echo after-restart; ls /sandbox"))
	show("bash after restart", out, err)
	return err
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
