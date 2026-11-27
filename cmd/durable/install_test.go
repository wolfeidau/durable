package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestInstallScriptFallsBackWhenGHUnauthenticated(t *testing.T) {
	target := runtime.GOOS + "_" + runtime.GOARCH
	switch target {
	case "darwin_arm64", "linux_amd64", "linux_arm64":
	default:
		t.Skipf("install.sh does not publish target %s", target)
	}

	assetsDir := t.TempDir()
	asset := fmt.Sprintf("durable_%s.tar.gz", target)
	archiveBytes := buildFakeReleaseArchive(t)
	require.NoError(t, os.WriteFile(filepath.Join(assetsDir, asset), archiveBytes, 0o600))

	sum := sha256.Sum256(archiveBytes)
	// Match goreleaser split checksum format: bare hex digest with no trailing newline.
	require.NoError(t, os.WriteFile(filepath.Join(assetsDir, asset+".sha256"), []byte(hex.EncodeToString(sum[:])), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(assetsDir, asset+".sigstore.json"), []byte("{}"), 0o600))

	shimDir := t.TempDir()
	writeShim(t, shimDir, "gh", `#!/bin/sh
if [ "${1:-}" = "auth" ] && [ "${2:-}" = "status" ]; then
  echo "You are not logged into any GitHub hosts." >&2
  exit 1
fi
echo "unexpected gh invocation: $*" >&2
exit 99
`)
	writeShim(t, shimDir, "curl", fmt.Sprintf(`#!/bin/sh
set -eu
out=""
url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
cp %q/"$(basename "${url}")" "${out}"
`, assetsDir))

	prefix := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "../../install.sh", "--version", "v1.2.3", "--prefix", prefix)
	cmd.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + shimDir + ":/usr/bin:/bin",
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	require.Contains(t, string(out), "verified: checksum")
	require.Contains(t, string(out), "installed durable 1.2.3 (test, now) and durable-server to "+filepath.Join(prefix, "bin"))

	for _, name := range []string{"durable", "durable-server"} {
		st, statErr := os.Stat(filepath.Join(prefix, "bin", name))
		require.NoError(t, statErr)
		require.NotZero(t, st.Mode().Perm()&0o111)
	}

	// Verify checksum mismatch aborts before installing.
	require.NoError(t, os.WriteFile(filepath.Join(assetsDir, asset+".sha256"), []byte("0000000000000000000000000000000000000000000000000000000000000000"), 0o600))
	badPrefix := t.TempDir()
	badCmd := exec.CommandContext(ctx, "/bin/sh", "../../install.sh", "--version", "v1.2.3", "--prefix", badPrefix)
	badCmd.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + shimDir + ":/usr/bin:/bin",
	}
	badOut, err := badCmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(badOut), "checksum mismatch")
}

func buildFakeReleaseArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"durable":        "#!/bin/sh\necho '1.2.3 (test, now)'\n",
		"durable-server": "#!/bin/sh\necho '1.2.3 (test, now)'\n",
	}
	for name, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o755,
			Size: int64(len(body)),
		}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func writeShim(t *testing.T, dir, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700))
}
