package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/tmux"
)

// cmdUpgrade puts a build of laatmux on a host and restarts its daemon:
// the binary is built for the host's platform from the checkout, or
// taken from --bin, sent over the same ssh alias the client uses, written
// next to the configured binary and renamed into place, and the new
// binary's stop is run there, which ends the old daemon cleanly so its
// runs are cancelled and waited for. The next connection starts the new
// daemon; the last step is that connection, which prints the version.
// The local host is upgraded the same way, in place of the running
// executable. A host that is not reachable is reported and skipped.
func cmdUpgrade(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	src := fs.String("src", "", "laatmux checkout to build from; default the current directory when it is one")
	bin := fs.String("bin", "", "a built binary to install instead of building; it must be for the host's platform")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: laatmux upgrade <host>... [--src dir] [--bin file]")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var hosts []config.Host
	for _, name := range fs.Args() {
		h, ok := cfg.Find(name)
		if !ok {
			return fmt.Errorf("unknown host %q", name)
		}
		hosts = append(hosts, h)
	}
	b := &builder{src: *src, bin: *bin, built: map[string]string{}}
	defer b.cleanup()
	failed := 0
	for _, h := range hosts {
		if err := upgradeHost(ctx, h, b); err != nil {
			fmt.Fprintf(os.Stderr, "laatmux: %s: %v\n", h.Name, err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d hosts not upgraded", failed, len(hosts))
	}
	return nil
}

// upgradeHost upgrades one host: find it, build for it, install, stop
// the old daemon, connect to the new one.
func upgradeHost(ctx context.Context, h config.Host, b *builder) error {
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	c, err := client.Dial(dctx, h.Host)
	cancel()
	if err != nil {
		return err
	}
	was := c.Hello.Version
	c.Close()
	var goos, goarch string
	if h.Local() {
		goos, goarch = runtime.GOOS, runtime.GOARCH
	} else {
		out, err := sshOutput(ctx, h.Host.SSH, "uname -sm")
		if err != nil {
			return fmt.Errorf("platform: %w", err)
		}
		if goos, goarch, err = parsePlatform(out); err != nil {
			return err
		}
	}
	file, version, err := b.build(ctx, goos, goarch)
	if err != nil {
		return err
	}
	fmt.Printf("%s: installing %s for %s/%s (daemon was %s)\n", h.Name, version, goos, goarch, was)
	if h.Local() {
		if err := installLocal(ctx, file); err != nil {
			return err
		}
	} else {
		if err := installRemote(ctx, h.Host, file); err != nil {
			return err
		}
	}
	dctx, cancel = context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err = client.Dial(dctx, h.Host)
	if err != nil {
		return fmt.Errorf("installed, but the new daemon did not answer: %w", err)
	}
	defer c.Close()
	fmt.Printf("%s: daemon %s\n", h.Name, c.Hello.Version)
	return nil
}

// builder builds laatmux once per platform for one upgrade run, in a
// temporary directory, or hands out the binary given with --bin.
type builder struct {
	src, bin string
	dir      string
	built    map[string]string // "goos/goarch" -> file
	version  string
}

// build returns a binary for the platform and the version it carries.
func (b *builder) build(ctx context.Context, goos, goarch string) (file, version string, err error) {
	if b.bin != "" {
		return b.bin, "the given binary", nil
	}
	key := goos + "/" + goarch
	if f, ok := b.built[key]; ok {
		return f, b.version, nil
	}
	src, err := b.source()
	if err != nil {
		return "", "", err
	}
	if b.dir == "" {
		if b.dir, err = os.MkdirTemp("", "laatmux-upgrade-"); err != nil {
			return "", "", err
		}
	}
	if b.version == "" {
		b.version = describe(ctx, src)
	}
	file = filepath.Join(b.dir, "laatmux-"+goos+"-"+goarch)
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags", "-X main.version="+b.version, "-o", file, "./cmd/laatmux")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("go build for %s in %s: %v\n%s", key, src, err, strings.TrimSpace(string(out)))
	}
	b.built[key] = file
	return file, b.version, nil
}

// source is the checkout to build from: --src, else the current
// directory when its go.mod is laatmux's.
func (b *builder) source() (string, error) {
	if b.src != "" {
		return b.src, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	mod, err := os.ReadFile(filepath.Join(cwd, "go.mod"))
	if err != nil || !bytes.HasPrefix(mod, []byte("module github.com/laat/laatmux\n")) {
		return "", errors.New("not in the laatmux checkout; run upgrade there, or pass --src <checkout> or --bin <binary>")
	}
	return cwd, nil
}

func (b *builder) cleanup() {
	if b.dir != "" {
		os.RemoveAll(b.dir)
	}
}

// describe is the version a build carries: git describe of the
// checkout, or dev when git cannot say.
func describe(ctx context.Context, src string) string {
	cmd := exec.CommandContext(ctx, "git", "-C", src, "describe", "--always", "--dirty")
	out, err := cmd.Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return "dev"
	}
	return string(bytes.TrimSpace(out))
}

// parsePlatform reads uname -sm into GOOS and GOARCH.
func parsePlatform(unameSM string) (goos, goarch string, err error) {
	f := strings.Fields(unameSM)
	if len(f) != 2 {
		return "", "", fmt.Errorf("uname -sm said %q", strings.TrimSpace(unameSM))
	}
	switch strings.ToLower(f[0]) {
	case "linux":
		goos = "linux"
	case "darwin":
		goos = "darwin"
	default:
		return "", "", fmt.Errorf("unsupported system %q", f[0])
	}
	switch f[1] {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	default:
		return "", "", fmt.Errorf("unsupported machine %q", f[1])
	}
	return goos, goarch, nil
}

// sshOutput runs a command on the host and returns its stdout.
func sshOutput(ctx context.Context, alias, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh", "-T", "-o", "BatchMode=yes", alias, command)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("ssh %s: %s", alias, msg)
		}
		return "", fmt.Errorf("ssh %s: %w", alias, err)
	}
	return string(out), nil
}

// installRemote streams the binary to the host over ssh and runs the
// install script there, which puts it in place and stops the old daemon.
func installRemote(ctx context.Context, h client.Host, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, "ssh", "-T", "-o", "BatchMode=yes", h.SSH, tmux.ShellJoin([]string{"sh", "-c", installScript(h.Bin)}))
	cmd.Stdin = f
	cmd.Stdout = os.Stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("install: %s", msg)
		}
		return fmt.Errorf("install: %w", err)
	}
	return nil
}

// installScript is the sh script that installs the binary read from
// stdin at the configured path on the host, then stops the daemon with
// the new binary. A path under ~ is the remote home; a bare name is
// found on the remote PATH, as the bridge finds it. The file is written
// beside the old one and renamed over it, so a running daemon keeps its
// own inode and the install is atomic; there is never a half-written
// binary at the path, and no text-file-busy since nothing is written
// into the old file.
func installScript(bin string) string {
	if bin == "" {
		bin = "laatmux"
	}
	var target string
	switch {
	case strings.HasPrefix(bin, "~/"):
		target = `bin="$HOME"/` + tmux.ShellJoin([]string{strings.TrimPrefix(bin, "~/")})
	case strings.Contains(bin, "/"):
		target = "bin=" + tmux.ShellJoin([]string{bin})
	default:
		q := tmux.ShellJoin([]string{bin})
		target = `bin=$(command -v ` + q + `) || { echo ` + q + ` is not on the PATH of a non-interactive shell; set bin in the host config >&2; exit 1; }`
	}
	return strings.Join([]string{
		"set -e",
		target,
		`mkdir -p "$(dirname "$bin")"`,
		`cat > "$bin.new"`,
		`chmod +x "$bin.new"`,
		`mv -f "$bin.new" "$bin"`,
		`"$bin" stop`,
	}, "; ")
}

// installLocal puts the binary in place of the running executable, then
// stops this machine's daemon.
func installLocal(ctx context.Context, file string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	in, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if err := os.WriteFile(exe+".new", in, 0o755); err != nil {
		return err
	}
	if err := os.Rename(exe+".new", exe); err != nil {
		os.Remove(exe + ".new")
		return err
	}
	fmt.Printf("installed at %s\n", exe)
	return cmdStop(ctx, nil)
}
