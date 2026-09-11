package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mehrad-meraji/bosun/internal/backup"
	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/sock"
	"github.com/mehrad-meraji/bosun/internal/state"
)

var help = map[string]string{
	"status": "bosun status\n  Show watched containers: mode, image, last update, downtime, and whether a rollback is kept.\n" +
		"  Example: docker exec -it bosun-gate bosun status",
	"check": "bosun check [--dry-run]\n  Run an update round now. --dry-run only prints what it would do.\n" +
		"  Example: docker exec -it bosun-gate bosun check --dry-run",
	"rollback": "bosun rollback ls\nbosun rollback show <name> [--with-data]\nbosun rollback <name> [--with-data] [--dry-run] [--yes]\n" +
		"  Go back to the version kept by the last update. --with-data also puts the volumes back from\n" +
		"  the backup made before that update (containers with bosun.backup=true). It asks before it acts.\n" +
		"  Example: docker exec -it bosun-gate bosun rollback nginx --with-data",
	"skip": "bosun skip ls\nbosun skip clear <name>\n  List or clear versions Bosun will not update to.\n" +
		"  Example: docker exec -it bosun-gate bosun skip clear nginx",
}

func usage() {
	fmt.Println("usage: bosun <status|check|rollback|skip|help> ...\nRun `bosun help <command>` for one command.")
}

func cmdHelp(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	h, ok := help[args[0]]
	if !ok {
		return fmt.Errorf("no help for %q. Commands: status, check, rollback, skip", args[0])
	}
	fmt.Println(h)
	return nil
}

// splitArgs separates --flags from names, so `rollback nginx --yes` works.
// ponytail: no flag values are needed yet; add them when a flag takes one.
func splitArgs(args []string) ([]string, map[string]bool) {
	var pos []string
	flags := map[string]bool{}
	for _, a := range args {
		if f, ok := strings.CutPrefix(a, "--"); ok {
			flags[f] = true
		} else {
			pos = append(pos, a)
		}
	}
	return pos, flags
}

func checkFlags(flags map[string]bool, allowed ...string) error {
	for f := range flags {
		if !slices.Contains(allowed, f) {
			return fmt.Errorf("unknown flag --%s. Run `bosun help`", f)
		}
	}
	return nil
}

func cmdStatus(ctx context.Context) error {
	ws, err := newGate().List(ctx)
	if err != nil {
		return err
	}
	if len(ws) == 0 {
		fmt.Println("No watched containers. Add the label bosun.enable=true to a container.")
		return nil
	}
	st, err := state.Read(stateDir)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tMODE\tIMAGE\tLAST UPDATE\tDOWNTIME\tBACKUP\tROLLBACK KEPT")
	for _, w := range ws {
		e := st.Entry(w.Name)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", w.Name, w.Mode, w.Ref, ago(e.UpdatedAt), dur(e.Downtime), backupCol(w), yesNo(e.Prev != ""))
	}
	return tw.Flush()
}

// backupCol says whether backups are on and how big the last one is.
func backupCol(w gate.Watched) string {
	if !w.Backup {
		return "off"
	}
	m, err := backup.ReadManifest(filepath.Join(backupDir, w.Name))
	if err != nil {
		return "on, none yet"
	}
	return "on, " + backup.FormatSize(m.Bytes())
}

func cmdCheck(ctx context.Context, args []string) error {
	pos, flags := splitArgs(args)
	if err := checkFlags(flags, "dry-run"); err != nil {
		return err
	}
	if len(pos) > 0 {
		return errors.New("check takes no names. Run `bosun help check`")
	}
	var lines []string
	c := sock.Client(filepath.Join(runDir, "updater.sock"))
	if err := sock.Post(ctx, c, "http://updater/round", map[string]bool{"dry_run": flags["dry-run"]}, &lines); err != nil {
		return fmt.Errorf("ask the updater: %w. Is it running? See `docker logs bosun-updater`", err)
	}
	if len(lines) == 0 {
		fmt.Println("No watched containers. Add the label bosun.enable=true to a container.")
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return nil
}

// ponytail: a container named "ls" or "show" cannot be rolled back by name.
func cmdRollback(ctx context.Context, args []string) error {
	pos, flags := splitArgs(args)
	if err := checkFlags(flags, "dry-run", "yes", "with-data"); err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("which container? Run `bosun rollback ls` to see what you can roll back")
	}
	st, err := state.Read(stateDir)
	if err != nil {
		return err
	}
	switch pos[0] {
	case "ls":
		return rollbackLs(ctx, st)
	case "show":
		if len(pos) < 2 {
			return errors.New("usage: bosun rollback show <name>")
		}
		return rollbackShow(ctx, st, pos[1], flags["with-data"])
	}
	name := pos[0]
	if err := rollbackShow(ctx, st, name, flags["with-data"]); err != nil {
		return err
	}
	if flags["dry-run"] {
		fmt.Println("Dry run: nothing changed.")
		return nil
	}
	if !flags["yes"] && !confirm("Continue? [y/N] ") {
		return errors.New("stopped; nothing changed")
	}
	res, err := newGate().Rollback(ctx, name, flags["with-data"])
	if err != nil {
		return err
	}
	fmt.Println(res.Message)
	if res.Status == gate.StatusReverted {
		return errors.New("the rollback did not happen")
	}
	return nil
}

func rollbackLs(ctx context.Context, st *state.State) error {
	d := docker.New(dockerSock)
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tNOW\tBACK TO\tUPDATED\tDATA BACKUP")
	n := 0
	for _, name := range sortedNames(st) {
		e := st.Containers[name]
		if e.Prev == "" {
			continue
		}
		now := "?"
		if c, err := d.Inspect(ctx, name); err == nil {
			now = c.Config.Image + " (" + short(c.Image) + ")"
		}
		data := "no"
		if m, err := newGate().DataBackup(ctx, name, e.Prev); err == nil {
			data = "yes, " + backup.FormatSize(m.Bytes())
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, now, e.Prev, ago(e.UpdatedAt), data)
		n++
	}
	if n == 0 {
		fmt.Println("Nothing to roll back yet. Bosun keeps one old version after each update.")
		return nil
	}
	return tw.Flush()
}

func rollbackShow(ctx context.Context, st *state.State, name string, withData bool) error {
	e := st.Containers[name]
	if e == nil || e.Prev == "" {
		return fmt.Errorf("no old version kept for %s. Run `bosun rollback ls` to see what you can roll back", name)
	}
	c, err := docker.New(dockerSock).Inspect(ctx, name)
	if err != nil {
		return err
	}
	m, dataErr := newGate().DataBackup(ctx, name, e.Prev)
	fmt.Printf("%s now runs %s (image %s).\n", name, c.Config.Image, short(c.Image))
	fmt.Printf("A rollback puts back %s, from the update %s.\n", e.Prev, ago(e.UpdatedAt))
	switch {
	case withData && dataErr != nil:
		return dataErr
	case withData:
		fmt.Printf("With --with-data, the volumes go back to the backup from %s (%s). Data written since then is lost.\n",
			ago(m.Time), backup.FormatSize(m.Bytes()))
		fmt.Printf("A short helper container does this. It runs as root, with no network. It only sees %s's folders and the backup folder (read-only).\n", name)
	case dataErr == nil:
		fmt.Printf("Volumes are not changed. A data backup from %s is kept; add --with-data to put it back too.\n", ago(m.Time))
	default:
		fmt.Println("Volumes are not changed. Data written by the newer version stays.")
	}
	fmt.Println("The newer version goes on the skip list.")
	flag := ""
	if withData {
		flag = " --with-data"
	}
	fmt.Printf("To do it: docker exec -it bosun-gate bosun rollback %s%s\n", name, flag)
	return nil
}

func cmdSkip(args []string) error {
	pos, flags := splitArgs(args)
	if err := checkFlags(flags); err != nil {
		return err
	}
	switch {
	case len(pos) == 1 && pos[0] == "ls":
		st, err := state.Read(stateDir)
		if err != nil {
			return err
		}
		n := 0
		for _, name := range sortedNames(st) {
			for _, d := range st.Containers[name].Skip {
				fmt.Printf("%s\t%s\n", name, d)
				n++
			}
		}
		if n == 0 {
			fmt.Println("Nothing is on the skip list.")
		}
		return nil
	case len(pos) == 2 && pos[0] == "clear":
		f, st, err := state.Open(stateDir, false)
		if err != nil {
			return err
		}
		defer f.Close()
		e := st.Containers[pos[1]]
		if e == nil || len(e.Skip) == 0 {
			return fmt.Errorf("nothing is skipped for %s. Run `bosun skip ls`", pos[1])
		}
		e.Skip = nil
		if err := f.Save(st); err != nil {
			return err
		}
		fmt.Printf("%s: skip list cleared. The next round may update it again.\n", pos[1])
		return nil
	}
	return errors.New("usage: bosun skip ls | bosun skip clear <name>")
}

func confirm(q string) bool {
	fmt.Print(q)
	a, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a = strings.ToLower(strings.TrimSpace(a))
	return a == "y" || a == "yes"
}

func sortedNames(st *state.State) []string {
	names := make([]string, 0, len(st.Containers))
	for n := range st.Containers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func dur(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	return d.Round(100 * time.Millisecond).String()
}

func short(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
