// Command bosun keeps Docker containers up to date without giving the
// network-facing code the Docker socket.
// Design: docs/superpowers/specs/2026-09-11-bosun-design.md
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mehrad-meraji/bosun/internal/backup"
	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/registry"
	"github.com/mehrad-meraji/bosun/internal/sock"
	"github.com/mehrad-meraji/bosun/internal/updater"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var (
	dockerSock = env("BOSUN_DOCKER_SOCK", "/var/run/docker.sock")
	runDir     = env("BOSUN_RUN_DIR", "/run/bosun")
	stateDir   = filepath.Clean(env("BOSUN_STATE_DIR", "/var/lib/bosun"))
	backupDir  = filepath.Clean(env("BOSUN_BACKUP_DIR", "/var/lib/bosun-backups"))
	warnSize   = env("BOSUN_BACKUP_WARN_SIZE", "10GB")
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "bosun:", err)
		if errors.Is(err, backup.ErrUntouched) {
			os.Exit(3) // the restore helper changed nothing; not 2, which a Go crash uses
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd string, args []string) error {
	switch cmd {
	case "gate":
		return runGate(ctx)
	case "updater":
		return runUpdater(ctx)
	case "status":
		return cmdStatus(ctx)
	case "check":
		return cmdCheck(ctx, args)
	case "rollback":
		return cmdRollback(ctx, args)
	case "skip":
		return cmdSkip(args)
	case "restore-helper":
		// A crash must not look like "nothing was changed"; Go exits 2 on a panic.
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintln(os.Stderr, "bosun: restore helper crashed:", r)
				os.Exit(1)
			}
		}()
		return backup.Restore(args)
	case "help", "-h", "--help":
		return cmdHelp(args)
	}
	return fmt.Errorf("unknown command %q. Run `bosun help`", cmd)
}

func newGate() *gate.Gate {
	self, _ := os.Hostname()              // Docker sets it to the short container ID
	warn, _ := backup.ParseSize(warnSize) // checked at gate start by checkSettings
	return &gate.Gate{D: docker.New(dockerSock), Dir: stateDir, RunDir: runDir, BackupDir: backupDir,
		WarnSize: warn, SelfID: self}
}

// checkSettings refuses settings that would hand gate-only folders to the
// updater, which gets RunDir and /etc/bosun.
func checkSettings() error {
	if _, err := backup.ParseSize(warnSize); err != nil {
		return fmt.Errorf("BOSUN_BACKUP_WARN_SIZE: %w", err)
	}
	for _, d := range []struct{ name, path string }{{"BOSUN_STATE_DIR", stateDir}, {"BOSUN_BACKUP_DIR", backupDir}} {
		for _, shared := range []string{runDir, "/etc/bosun"} {
			if inside(d.path, shared) {
				return fmt.Errorf("%s (%s) is inside %s, which the updater can read; pick a folder outside it", d.name, d.path, shared)
			}
		}
	}
	return nil
}

func inside(child, parent string) bool {
	child, parent = filepath.Clean(child), filepath.Clean(parent)
	return child == parent || strings.HasPrefix(child, parent+"/")
}

func runGate(ctx context.Context) error {
	if err := checkSettings(); err != nil {
		return err
	}
	g := newGate()
	if err := g.Recover(ctx); err != nil {
		return fmt.Errorf("crash recovery: %w", err)
	}
	// Listen before starting the updater, so its first call waits for us.
	l, err := sock.Listen(filepath.Join(runDir, "gate.sock"))
	if err != nil {
		return err
	}
	if err := g.SpawnUpdater(ctx); err != nil {
		return fmt.Errorf("start the updater: %w", err)
	}
	defer func() {
		if err := g.RemoveUpdaters(context.Background()); err != nil {
			log.Printf("remove the updater: %v", err)
		}
	}()
	log.Printf("gate ready")
	return g.Serve(ctx, l)
}

func runUpdater(ctx context.Context) error {
	reg, err := registry.New(splitList(os.Getenv("BOSUN_INSECURE_REGISTRIES")), os.Getenv("BOSUN_CA_FILE"))
	if err != nil {
		return err
	}
	urls, err := updater.LoadURLs(env("BOSUN_NOTIFY_FILE", "/etc/bosun/notify.txt"))
	if err != nil {
		return err
	}
	u := &updater.Updater{Gate: gate.Dial(filepath.Join(runDir, "gate.sock")), Reg: reg, Notify: updater.Sender(urls)}
	l, err := sock.Listen(filepath.Join(runDir, "updater.sock"))
	if err != nil {
		return err
	}
	go func() {
		if err := u.Serve(ctx, l); err != nil {
			log.Printf("updater socket: %v", err)
		}
	}()
	schedule := env("BOSUN_SCHEDULE", "0 4 * * *")
	log.Printf("updater ready: %d notify URLs, schedule %q", len(urls), schedule)
	return u.Run(ctx, schedule)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
