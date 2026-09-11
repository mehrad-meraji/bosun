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
	stateDir   = env("BOSUN_STATE_DIR", "/var/lib/bosun")
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
			os.Exit(2) // the restore helper changed nothing
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
		return backup.Restore(args)
	case "help", "-h", "--help":
		return cmdHelp(args)
	}
	return fmt.Errorf("unknown command %q. Run `bosun help`", cmd)
}

func newGate() *gate.Gate {
	self, _ := os.Hostname() // Docker sets it to the short container ID
	return &gate.Gate{D: docker.New(dockerSock), Dir: stateDir, RunDir: runDir, SelfID: self}
}

func runGate(ctx context.Context) error {
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
