# Bosun Control Server Link Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bosun reports what it did to a control server as JSON events, and pulls a two-command list (`check`, `skip_clear`) from it, with no open port and no new power for a hacked server.

**Architecture:** Only the updater talks to the control server; the gate still has no network and opens no port. A new `internal/control` package holds the event type, the sender (3 retries) and the command reader (strict decode, replay guard). The updater builds events from the gate's replies and its own round, queues them, and a single goroutine sends them so a slow server never blocks an update. Commands arrive on a poll ticker; `check` runs a round in the updater, `skip_clear` goes to the gate as a new typed RPC, because the state file belongs to the gate.

**Tech Stack:** Go 1.27, stdlib `net/http` and `crypto/rand` only. No new outside libraries.

**Spec:** `docs/superpowers/specs/2026-09-11-bosun-design.md` (sections "Control server link", "Link settings", the `skipClear(name)` row in the RPC table, and the "Control server link" bullet under Tests)

## Global Constraints

- Go 1.27. Module `github.com/mehrad-meraji/bosun`. **No new outside libraries** — stdlib only for everything in this plan.
- Docker Engine API pinned to `v1.44`.
- **The gate has no network and opens no port.** The updater makes every outside call. The control server never connects to Bosun. Nothing in this plan gives the gate a network client or the updater the Docker socket.
- The event body never holds registry logins, notify URLs or env vars. The bearer token is never written to a log or into an error string.
- Only two commands exist: `check` and `skip_clear`. There is no remote `update`, `rollback` or restore, ever.
- A failed event never blocks or delays an update. Same rule as notes.
- Events are in memory only. A restart loses events that were not sent. That is on purpose.
- `BOSUN_CONTROL_TOKEN_FILE` default `/etc/bosun/control-token`. It must live under `/etc/bosun`, the only folder the gate shares with the updater (read-only).
- `BOSUN_CONTROL_POLL` default `60s`, lowest value `15s`.
- `BOSUN_CONTROL_URL` must be `https://`, unless `BOSUN_CONTROL_INSECURE=true`.
- User-facing text uses plain, short English. Every error says what to do next.
- Run after every task: `go build ./... && go vet ./... && go vet -tags integration ./... && go test ./...`
- Commit in logical commits. End each commit message with a blank line then `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.

## File Structure

| File | Responsibility |
|---|---|
| `internal/sock/sock.go` (modify) | `Post` returns a typed `*HTTPError` so a caller can tell a refusal (403) from a failure. |
| `internal/docker/docker.go` (modify) | New `Info(ctx)` reads the Docker host name (`GET /info` → `Name`). |
| `internal/gate/boot.go` (modify) | `SpawnUpdater` puts `BOSUN_HOST` in the updater's env when the user did not set it. |
| `internal/gate/gate.go` (modify) | `Step` type, the `stepper` recorder, `Result.Steps`, `SkipClear`, `IsRefused`. |
| `internal/gate/rpc.go` (modify) | `/skip-clear` handler, request type, client method. |
| `internal/control/control.go` (create) | `Version`, `Event`, `Backup`, `Client`, `New`, `Send` (retries), `post`. |
| `internal/control/commands.go` (create) | `Command`, `Refusal`, `Commands`, pure `parse`, the 24-hour replay guard. |
| `internal/updater/events.go` (create) | Pure builders that turn a round outcome or a gate event into a `control.Event`. |
| `internal/updater/updater.go` (modify) | `Control`/`Host` fields, the event queue and its sender, `round(ctx, dryRun, cmdID)`. |
| `internal/updater/commands.go` (create) | The poll loop and `runCommand`. |
| `main.go` (modify) | The five `BOSUN_CONTROL_*` settings, their guards, and the wiring. |
| `cli.go` (modify) | `skip clear` reuses `Gate.SkipClear`. |
| `README.md`, `compose.yml`, the spec (modify) | Settings table, token mount, and the spec fixes this plan makes. |

---

### Task 1: Typed socket errors, the Docker host name, and `BOSUN_HOST`

The updater must label every event with the host and must tell a gate refusal (403) from a gate failure (500). Today `sock.Post` throws the status away.

**Files:**
- Modify: `internal/sock/sock.go` (the `Post` func at the end of the file)
- Modify: `internal/docker/docker.go` (add `Info` after `InspectImage`)
- Modify: `internal/gate/gate.go` (add `IsRefused` under `RefusedError`)
- Modify: `internal/gate/boot.go` (`SpawnUpdater` and `updaterBody`)
- Modify: `docs/superpowers/specs/2026-09-11-bosun-design.md`
- Test: `internal/sock/sock_test.go`, `internal/docker/docker_test.go`, `internal/gate/boot_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `sock.HTTPError{Status int; Msg string}` with `Error() string` returning `Msg`.
  - `gate.IsRefused(err error) bool` — true for a gate 403.
  - `gate.IsBusy(err error) bool` — true for `state.ErrBusy` or a gate 409.
  - `(*docker.Client).Info(ctx context.Context) (string, error)` — the Docker host name.
  - `gate.updaterBody(self *docker.Container, dir, host string) map[string]any` — third parameter is new.

- [ ] **Step 1: Write the failing tests**

Add to `internal/sock/sock_test.go`:

```go
func TestPostKeepsTheStatus(t *testing.T) {
	dir, err := os.MkdirTemp("", "bs") // macOS caps socket paths at 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s.sock")
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /no", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "refused: bad name", http.StatusForbidden)
	})
	go func() { _ = Serve(context.Background(), l, mux) }()

	err = Post(context.Background(), Client(path), "http://x/no", struct{}{}, &struct{}{})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusForbidden {
		t.Fatalf("err = %v (%T), want an HTTPError with status 403", err, err)
	}
	if err.Error() != "refused: bad name" {
		t.Errorf("Error() = %q, want the reply text unchanged", err.Error())
	}
}
```

Add to `internal/docker/docker_test.go`. `fake(t, h)` is that file's existing helper: it serves `h` on a unix socket and returns a `*Client` for it.

```go
func TestInfoReadsTheHostName(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+APIVersion+"/info" {
			t.Errorf("path = %s, want /info", r.URL.Path)
		}
		io.WriteString(w, `{"Name":"worker-1","OperatingSystem":"whatever"}`)
	})
	host, err := c.Info(context.Background())
	if err != nil || host != "worker-1" {
		t.Fatalf("Info = %q, %v, want worker-1", host, err)
	}
}
```

Add to `internal/gate/boot_test.go`:

```go
func TestUpdaterBodyCarriesTheHost(t *testing.T) {
	self := &docker.Container{Image: "sha256:img"}
	b, _ := json.Marshal(updaterBody(self, "/run/bosun", "worker-1"))
	if !strings.Contains(string(b), `"BOSUN_HOST=worker-1"`) {
		t.Errorf("updater body lacks the host: %s", b)
	}
}

func TestUpdaterBodyKeepsTheUsersHost(t *testing.T) {
	self := &docker.Container{Image: "sha256:img"}
	self.Config.Env = []string{"BOSUN_HOST=mine"}
	b, _ := json.Marshal(updaterBody(self, "/run/bosun", ""))
	s := string(b)
	if !strings.Contains(s, `"BOSUN_HOST=mine"`) || strings.Count(s, "BOSUN_HOST=") != 1 {
		t.Errorf("want the user's host once and only once: %s", s)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/sock/ ./internal/docker/ ./internal/gate/ -run 'TestPostKeepsTheStatus|TestInfoReadsTheHostName|TestUpdaterBody' -count=1`
Expected: FAIL — `HTTPError` undefined, `Info` undefined, `updaterBody` wants 2 arguments.

- [ ] **Step 3: Implement**

In `internal/sock/sock.go`, above `Post`:

```go
// HTTPError is a reply with a status other than 200. Msg is the reply text,
// so an error reads the same as before; Status lets a caller tell a refusal
// (403) from a failure (500).
type HTTPError struct {
	Status int
	Msg    string
}

func (e *HTTPError) Error() string { return e.Msg }
```

In `Post`, replace the non-200 branch:

```go
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &HTTPError{Status: resp.StatusCode, Msg: strings.TrimSpace(string(msg))}
	}
```

In `internal/docker/docker.go`, after `InspectImage`:

```go
// Info returns the Docker host's name, which Bosun uses as the host name in
// control-server events.
func (c *Client) Info(ctx context.Context) (string, error) {
	var out struct{ Name string }
	if err := c.call(ctx, http.MethodGet, "/info", nil, nil, &out); err != nil {
		return "", err
	}
	return out.Name, nil
}
```

In `internal/gate/gate.go`, under `RefusedError`'s methods (and add `"net/http"` and `"github.com/mehrad-meraji/bosun/internal/sock"` to the imports):

```go
// IsRefused reports whether the gate refused a call. The updater tells a
// refusal from a failure this way, because they are different events.
func IsRefused(err error) bool {
	var ref *RefusedError
	if errors.As(err, &ref) {
		return true
	}
	var he *sock.HTTPError
	return errors.As(err, &he) && he.Status == http.StatusForbidden
}

// IsBusy reports whether the gate was busy with an update. The caller may
// try again later, so it is not a failure. Over the socket it arrives as a
// 409, like the updater's own busy reply.
func IsBusy(err error) bool {
	if errors.Is(err, state.ErrBusy) {
		return true
	}
	var he *sock.HTTPError
	return errors.As(err, &he) && he.Status == http.StatusConflict
}
```

`internal/gate/gate_test.go` already has an unexported `isRefused`. Delete it and point its callers at the new `IsRefused`, so there is one check, not two.

In `internal/gate/boot.go`, in `SpawnUpdater`, after the `self, err := g.D.Inspect(...)` block:

```go
	// The host name labels every control-server event. Docker knows it; the
	// user can override it with BOSUN_HOST on the gate.
	host := ""
	if !hasEnv(self.Config.Env, "BOSUN_HOST") {
		if h, err := g.D.Info(ctx); err != nil {
			log.Printf("read the Docker host name: %v; events will have no host", err)
		} else {
			host = h
		}
	}
```

and pass it: `g.D.Create(ctx, UpdaterName, updaterBody(self, g.RunDir, host))`.

In `updaterBody`, change the signature to `func updaterBody(self *docker.Container, dir, host string) map[string]any` and after the env loop:

```go
	if host != "" {
		env = append(env, "BOSUN_HOST="+host)
	}
```

and add next to it:

```go
// hasEnv reports whether the list already sets key.
func hasEnv(list []string, key string) bool {
	for _, e := range list {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/sock/ ./internal/docker/ ./internal/gate/ -count=1`
Expected: PASS (the whole gate package too — `updaterBody` has other callers in tests that now need the third argument; fix those call sites with `""`).

- [ ] **Step 5: Fix the spec**

The spec says the host comes from the `list()` reply. It does not: it comes to the updater in its env, which keeps the gate's RPC surface unchanged. Edit `docs/superpowers/specs/2026-09-11-bosun-design.md`:

- In the line that starts "With the control server link, the `list()` reply also gives the Docker host name", drop the host half so it reads: "With the control server link, the `update` reply also gives a `steps` list."
- In the Events bullet list, replace the `host` bullet with: "- `host` comes from the gate. The gate reads Docker's host name once and puts it in the updater's env. `BOSUN_HOST` on the gate overrides it."

- [ ] **Step 6: Commit**

```bash
git add internal/sock internal/docker internal/gate docs/superpowers/specs
git commit -m "Typed socket errors, the Docker host name, and BOSUN_HOST for the updater"
```

---

### Task 2: `skipClear(name)` on the gate

The control server may clear a container's skip list. The state file is the gate's, so the gate does it, under the same lock as the CLI.

**Files:**
- Modify: `internal/gate/gate.go` (add `SkipClear` after `Rollback`)
- Modify: `internal/gate/rpc.go` (handler, request type, client method)
- Modify: `cli.go` (`cmdSkip`'s `clear` branch reuses it)
- Test: `internal/gate/rpc_test.go`, `internal/gate/gate_test.go`

**Interfaces:**
- Consumes: `state.Open(dir, false)`, `ErrBusy`, `nameRE`, `refuse` (all exist).
- Produces:
  - `gate.SkipClearRequest{Name string}` with `Validate() error`
  - `gate.SkipClearResult{Cleared int; Message string}` (json `cleared`, `message`)
  - `(*gate.Gate).SkipClear(name string) (SkipClearResult, error)`
  - `(*gate.Client).SkipClear(ctx context.Context, name string) (SkipClearResult, error)`

- [ ] **Step 1: Write the failing tests**

Add to `internal/gate/gate_test.go`:

```go
func TestSkipClear(t *testing.T) {
	f := swapFake(true)
	g := f.gate(t)
	setEntry(t, g, "app", state.Entry{Skip: []string{"sha256:d2", "sha256:d3"}})

	res, err := g.SkipClear("app")
	if err != nil || res.Cleared != 2 {
		t.Fatalf("SkipClear = %+v, %v, want 2 cleared", res, err)
	}
	st, err := state.Read(g.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Entry("app").Skip) != 0 {
		t.Errorf("skip list = %v, want empty", st.Entry("app").Skip)
	}

	// Nothing skipped is not an error: the server asked, and now it is clear.
	res, err = g.SkipClear("app")
	if err != nil || res.Cleared != 0 || !strings.Contains(res.Message, "nothing") {
		t.Errorf("SkipClear on a clean entry = %+v, %v, want 0 and a plain message", res, err)
	}
}

func TestSkipClearRefusesBadNames(t *testing.T) {
	g := swapFake(true).gate(t)
	for _, name := range []string{"", "../etc", "a b", strings.Repeat("x", 200)} {
		if _, err := g.SkipClear(name); !IsRefused(err) {
			t.Errorf("SkipClear(%q) = %v, want a refusal", name, err)
		}
	}
}

func TestSkipClearIsBusyWhileAnUpdateRuns(t *testing.T) {
	g := swapFake(true).gate(t)
	f, st, err := state.Open(g.Dir, true) // hold the lock, like a running update
	if err != nil {
		t.Fatal(err)
	}
	_ = st
	defer f.Close()
	if _, err := g.SkipClear("app"); !errors.Is(err, state.ErrBusy) {
		t.Errorf("err = %v, want state.ErrBusy", err)
	}
}
```

Add to `internal/gate/rpc_test.go`:

```go
func TestServeSkipClear(t *testing.T) {
	f := swapFake(true)
	g := f.gate(t)
	setEntry(t, g, "app", state.Entry{Skip: []string{"sha256:d2"}})
	c, stop := serveGate(t, g)
	defer stop()

	res, err := c.SkipClear(context.Background(), "app")
	if err != nil || res.Cleared != 1 {
		t.Fatalf("SkipClear = %+v, %v, want 1 cleared", res, err)
	}
	if _, err := c.SkipClear(context.Background(), "../etc"); !IsRefused(err) {
		t.Errorf("err = %v, want the client to see a refusal", err)
	}
}

// A busy gate must read as busy through the socket too, not as a failure:
// the control server drops a failed command but retries a busy one.
func TestServeSkipClearIsBusyOverTheSocket(t *testing.T) {
	f := swapFake(true)
	g := f.gate(t)
	lock, _, err := state.Open(g.Dir, true) // hold the lock, like a running update
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	c, stop := serveGate(t, g)
	defer stop()

	_, err = c.SkipClear(context.Background(), "app")
	if !IsBusy(err) {
		t.Errorf("err = %v, want IsBusy", err)
	}
}
```

`serveGate` does not exist yet. Add it to `internal/gate/rpc_test.go`:

```go
// serveGate runs g on a socket and returns a client for it.
// ponytail: os.MkdirTemp, not t.TempDir, because macOS caps socket paths at 104 bytes.
func serveGate(t *testing.T, g *Gate) (*Client, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "bg")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "g.sock")
	l, err := sock.Listen(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = g.Serve(ctx, l) }()
	return Dial(p), func() { cancel(); os.RemoveAll(dir) }
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gate/ -run 'SkipClear' -count=1`
Expected: FAIL — `g.SkipClear` undefined.

- [ ] **Step 3: Implement**

In `internal/gate/gate.go`, after `Rollback`:

```go
// SkipClearResult says how many versions came off a container's skip list.
type SkipClearResult struct {
	Cleared int    `json:"cleared"`
	Message string `json:"message"`
}

// SkipClear removes a container's versions from the skip list. It starts no
// update: the next round may try that version again. The state file belongs
// to the gate, so the CLI and the control server both come through here.
func (g *Gate) SkipClear(name string) (SkipClearResult, error) {
	if !nameRE.MatchString(name) {
		return SkipClearResult{}, refuse("bad container name %q", name)
	}
	f, st, err := state.Open(g.Dir, false)
	if err != nil {
		return SkipClearResult{}, err
	}
	defer f.Close()
	e := st.Containers[name]
	if e == nil || len(e.Skip) == 0 {
		return SkipClearResult{Message: fmt.Sprintf("%s: nothing was on the skip list", name)}, nil
	}
	n := len(e.Skip)
	e.Skip = nil
	res := SkipClearResult{Cleared: n, Message: fmt.Sprintf("%s: skip list cleared (%d). The next round may update it again", name, n)}
	return res, f.Save(st)
}
```

In `internal/gate/rpc.go`, next to `UpdateRequest`:

```go
// SkipClearRequest is the only other request with input.
type SkipClearRequest struct {
	Name string `json:"name"`
}

func (r SkipClearRequest) Validate() error {
	if !nameRE.MatchString(r.Name) {
		return refuse("bad container name %q", r.Name)
	}
	return nil
}
```

In `Serve`, after the `/update` handler:

```go
	mux.HandleFunc("POST /skip-clear", func(w http.ResponseWriter, r *http.Request) {
		var req SkipClearRequest
		err := decode(r.Body, &req)
		if err == nil {
			err = req.Validate()
		}
		if err != nil {
			reply(w, nil, err)
			return
		}
		res, err := g.SkipClear(req.Name)
		reply(w, res, err)
	})
```

In `internal/gate/rpc.go`, in `reply`, add the busy case before the plain error case, so a busy gate is a 409 and not a 500:

```go
	case errors.Is(err, state.ErrBusy):
		http.Error(w, err.Error(), http.StatusConflict)
```

Without it, `gate.IsBusy` can never see a busy gate over the socket, and the control server would be told a command failed when it should try again.

and the client method next to `Update`:

```go
func (c *Client) SkipClear(ctx context.Context, name string) (SkipClearResult, error) {
	var r SkipClearResult
	err := sock.Post(ctx, c.http, "http://gate/skip-clear", SkipClearRequest{Name: name}, &r)
	return r, err
}
```

- [ ] **Step 4: Make the CLI use it (DRY)**

In `cli.go`, replace the body of the `case len(pos) == 2 && pos[0] == "clear":` branch with:

```go
	case len(pos) == 2 && pos[0] == "clear":
		res, err := newGate().SkipClear(pos[1])
		if err != nil {
			return err
		}
		fmt.Println(res.Message + ".")
		return nil
```

`cmdSkip` now needs no `state.Open`; drop imports that go unused. The old text said "nothing is skipped for X" as an error; the new message is not an error, which is what the control server needs too. Update any `cli_test.go` case that expected that error.

- [ ] **Step 5: Run the tests**

Run: `go build ./... && go vet ./... && go test ./internal/gate/ ./ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/gate cli.go
git commit -m "Gate: skipClear RPC, shared by the CLI and the control server link"
```

---

### Task 3: `steps` in the update reply

The control server wants the update sequence, step by step. The gate records it; the reply carries it.

**Files:**
- Modify: `internal/gate/gate.go` (`Step`, `stepper`, `Result.Steps`, `Update`, `swapOpts`, `swap`)
- Test: `internal/gate/gate_test.go`

**Interfaces:**
- Consumes: `swapOpts`, `Result` (exist).
- Produces:
  - `gate.Step{Name, Status string; Ms int64; Detail string}` (json `name`, `status`, `ms,omitempty`, `detail,omitempty`)
  - `Result.Steps []Step` (json `steps,omitempty`)
  - Step names in order: `pull`, `stop`, `backup`, `start`, `health`, `rollback`, `skip`. Statuses: `ok`, `failed`, `skipped`.
  - Rule: the gate fills `Steps` only when it returns a `Result`. When `Update` returns an error instead (pull failed, digest moved), the updater makes up a single `pull` step itself — see Task 6.

- [ ] **Step 1: Write the failing test**

Add to `internal/gate/gate_test.go`:

```go
func TestUpdateRecordsSteps(t *testing.T) {
	f := swapFake(true)
	g := f.gate(t)
	res, err := g.Update(context.Background(), "app", "sha256:d2", "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range res.Steps {
		got = append(got, s.Name+":"+s.Status)
	}
	want := []string{"pull:ok", "stop:ok", "backup:skipped", "start:ok", "health:ok"}
	if !slices.Equal(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
}

func TestUpdateRevertedRecordsFailedSteps(t *testing.T) {
	f := swapFake(false) // the new container does not stay up
	g := f.gate(t)
	res, err := g.Update(context.Background(), "app", "sha256:d2", "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range res.Steps {
		got = append(got, s.Name+":"+s.Status)
	}
	want := []string{"pull:ok", "stop:ok", "backup:skipped", "start:ok", "health:failed", "rollback:ok", "skip:ok"}
	if !slices.Equal(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
	for _, s := range res.Steps {
		if s.Name == "health" && s.Detail == "" {
			t.Error("the failed health step must carry a short detail")
		}
	}
}

func TestBackupStepIsRecorded(t *testing.T) {
	f, g := backupFake(t, true)
	_ = f
	res, err := g.Update(context.Background(), "app", "sha256:d2", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Steps {
		if s.Name == "backup" {
			if s.Status != "ok" {
				t.Errorf("backup step = %+v, want ok", s)
			}
			return
		}
	}
	t.Errorf("no backup step in %+v", res.Steps)
}
```

`backupFake(t, true)` is the helper in `internal/gate/backup_test.go`; read it first and give the container the `bosun.backup=true` label the same way that file's tests do.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gate/ -run 'Steps|BackupStep' -count=1`
Expected: FAIL — `res.Steps` undefined.

- [ ] **Step 3: Implement the recorder**

In `internal/gate/gate.go`, next to `Result`:

```go
// Step is one step of an update, for the control server link. The CLI does
// not print steps; only the link sends them.
type Step struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "ok", "failed" or "skipped"
	Ms     int64  `json:"ms,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// stepper records the update sequence. Every method is safe on a nil
// stepper, so a rollback can pass nil and record nothing.
type stepper struct {
	list []Step
	t    time.Time
}

func (s *stepper) add(name, status, detail string) {
	if s == nil {
		return
	}
	now := time.Now()
	s.list = append(s.list, Step{Name: name, Status: status, Ms: now.Sub(s.t).Milliseconds(), Detail: detail})
	s.t = now
}

// skip records a step that did not run, so it has no time.
func (s *stepper) skip(name, detail string) {
	if s == nil {
		return
	}
	s.list = append(s.list, Step{Name: name, Status: "skipped", Detail: detail})
	s.t = time.Now()
}

func (s *stepper) steps() []Step {
	if s == nil {
		return nil
	}
	return s.list
}
```

Add to `Result`: `Steps []Step \`json:"steps,omitempty"\`` and to `swapOpts`: `steps *stepper`.

- [ ] **Step 4: Record the steps**

In `Update`: make the stepper before the pull, mark the pull, pass it to `swap`, and hang the list on every `Result` that comes back.

```go
	sp := &stepper{t: time.Now()}
	if err := g.D.Pull(ctx, ref, auth); err != nil {
		return Result{}, err
	}
	sp.add("pull", "ok", "")
```

then `swapOpts{digest: digest, backup: ..., steps: sp}`, and:

- in the `res.Status == StatusReverted` branch, after `e.AddSkip(digest)` and the retag: `sp.add("skip", "ok", digest+" added to the skip list")`, then `res.Steps = sp.steps()` before the `return res, f.Save(st)`;
- on the success path: `res.Steps = sp.steps()` before the final `return res, f.Save(st)`.

In `swap`, add one line at each point that already decides an outcome:

```go
	if err := g.D.Stop(ctx, old.ID); err != nil {
		opts.steps.add("stop", "failed", err.Error())
		return Result{}, fmt.Errorf("%s: stop: %w", name, err)
	}
	opts.steps.add("stop", "ok", "")
```

```go
	if opts.backup {
		b0 := time.Now()
		m, c, err := g.takeBackup(ctx, old)
		if err != nil {
			opts.steps.add("backup", "failed", err.Error())
			... // the two existing error returns, unchanged
		}
		took, size, commit = time.Since(b0), m.Bytes(), c
		opts.steps.add("backup", "ok", backup.FormatSize(size))
		...
	} else {
		opts.steps.skip("backup", "bosun.backup is off")
	}
```

```go
	newID, err := g.D.Create(ctx, name, body)
	if err == nil {
		err = g.D.Start(ctx, newID)
	}
	down := time.Since(t0)
	if err != nil {
		opts.steps.add("start", "failed", err.Error())
	} else {
		opts.steps.add("start", "ok", "")
		if err = g.waitHealthy(ctx, newID, timeoutOf(old)); err != nil {
			opts.steps.add("health", "failed", err.Error())
		} else {
			opts.steps.add("health", "ok", "")
		}
	}
```

(That replaces the `if err == nil { err = g.waitHealthy(...) }` line. Keep `err` as the variable the code below already reads.)

In the revert branch, after `revert` returns: `opts.steps.add("rollback", "ok", "")` when `rerr == nil`, and `opts.steps.add("rollback", "failed", rerr.Error())` when it does not. Put `Steps: opts.steps.steps()` in the `Result{Status: StatusReverted, ...}` literal and in the `Result{Status: StatusDone, ...}` literal at the end.

The rename failure paths get no step of their own: the `stop` step is already `ok` and the error text says what happened.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/gate/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/gate
git commit -m "Gate: record the update steps in the reply"
```

---

### Task 4: `internal/control` — the event type and the sender

**Files:**
- Create: `internal/control/control.go`
- Test: `internal/control/control_test.go`

**Interfaces:**
- Consumes: `gate.Step` (Task 3).
- Produces:
  - `control.Version` (string const), `control.Schema` (int const, 1)
  - `control.Backup{Bytes, Ms int64}` (json `bytes`, `ms`)
  - `control.Event` with the field set below
  - `control.New(rawURL, tokenFile, host string, insecure bool) (*Client, error)` — returns `(nil, nil)` when `rawURL` is empty: the link is off
  - `(*Client).Send(ctx context.Context, ev Event) error`
  - `(*Client).Host() string`
  - Event type strings: `EventUpdateDone = "update.done"`, `EventUpdateRolledBack = "update.rolled_back"`, `EventUpdateFailed = "update.failed"`, `EventVersionAvailable = "version.available"`, `EventGateRefused = "gate.refused"`, `EventRegistryFailing = "registry.failing"`, `EventRecovery = "recovery"`, `EventWarning = "warning"`, `EventCommandResult = "command.result"`

- [ ] **Step 1: Write the failing tests**

Create `internal/control/control_test.go`:

```go
package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mehrad-meraji/bosun/internal/gate"
)

// tokenFile writes a token file and returns its path.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "control-token")
	if err := os.WriteFile(p, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// testClient points a Client at s with retries made instant.
func testClient(t *testing.T, s *httptest.Server) *Client {
	t.Helper()
	c, err := New(s.URL, tokenFile(t, "secret-token"), "worker-1", true)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewIsOffWithoutAURL(t *testing.T) {
	c, err := New("", "/nope", "worker-1", false)
	if c != nil || err != nil {
		t.Fatalf("New(\"\") = %v, %v, want nil, nil: the link is off", c, err)
	}
}

func TestNewRefusesPlainHTTPAndBadSettings(t *testing.T) {
	tok := tokenFile(t, "t")
	for _, tc := range []struct{ name, url, token string }{
		{"plain http", "http://server", tok},
		{"no scheme", "server:8080", tok},
		{"missing token file", "https://server", filepath.Join(t.TempDir(), "gone")},
		{"empty token file", "https://server", tokenFile(t, "")},
	} {
		if _, err := New(tc.url, tc.token, "h", false); err == nil {
			t.Errorf("%s: want an error", tc.name)
		}
	}
	if _, err := New("https://server", tok, "", false); err == nil {
		t.Error("no host name: want an error, or every event goes out with an empty host")
	}
	if _, err := New("http://server", tok, "h", true); err != nil {
		t.Errorf("plain http with insecure = %v, want ok", err)
	}
}

func TestSendEvent(t *testing.T) {
	var body []byte
	var auth, ctype string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/events" {
			t.Errorf("got %s %s, want POST /events", r.Method, r.URL.Path)
		}
		body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		auth, ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
	}))
	defer s.Close()

	c := testClient(t, s)
	err := c.Send(context.Background(), Event{Type: EventUpdateDone, Container: "redis"})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer secret-token" || ctype != "application/json" {
		t.Errorf("headers = %q, %q", auth, ctype)
	}
	var got Event
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Schema != 1 || got.Version != Version || got.Host != "worker-1" || got.ID == "" || got.Time.IsZero() {
		t.Errorf("event = %+v, want the fixed fields filled in", got)
	}
	// Two events never share an id: the server drops repeats by id.
	first := got.ID
	if err := c.Send(context.Background(), Event{Type: EventUpdateDone}); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(body, &got)
	if got.ID == first {
		t.Error("two events share an id")
	}
}

func TestEventJSONIsTheShapeTheServerExpects(t *testing.T) {
	ev := Event{
		Schema:     Schema,
		ID:         "0f8c2c1e-5b7a-4d0e-9a51-3c2d7e6f1a90",
		Time:       time.Date(2026, 9, 11, 4, 1, 33, 0, time.UTC),
		Host:       "worker-1",
		Version:    "0.2.0",
		Type:       EventUpdateRolledBack,
		Container:  "redis",
		Image:      "redis:7.4",
		FromDigest: "sha256:b20c",
		ToDigest:   "sha256:9ae1",
		DowntimeMs: 38000,
		Steps: []gate.Step{
			{Name: "pull", Status: "ok", Ms: 6200},
			{Name: "backup", Status: "skipped", Detail: "bosun.backup is off"},
		},
		Reason: "health check failed",
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema":1,"id":"0f8c2c1e-5b7a-4d0e-9a51-3c2d7e6f1a90","time":"2026-09-11T04:01:33Z","host":"worker-1","bosun_version":"0.2.0","type":"update.rolled_back","container":"redis","image":"redis:7.4","from_digest":"sha256:b20c","to_digest":"sha256:9ae1","downtime_ms":38000,"steps":[{"name":"pull","status":"ok","ms":6200},{"name":"backup","status":"skipped","detail":"bosun.backup is off"}],"reason":"health check failed"}`
	if string(b) != want {
		t.Errorf("event JSON changed.\ngot:  %s\nwant: %s", b, want)
	}
}

func TestSendRetriesThenGivesUp(t *testing.T) {
	var tries atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tries.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer s.Close()

	old := retryWaits
	retryWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { retryWaits = old }()

	c := testClient(t, s)
	if err := c.Send(context.Background(), Event{Type: EventWarning}); err == nil {
		t.Fatal("want an error after the last try")
	}
	if n := tries.Load(); n != 4 {
		t.Errorf("tries = %d, want 4 (one, then three more)", n)
	}
}

func TestSendDoesNotRetryABadTokenAndNeverLeaksIt(t *testing.T) {
	var tries atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tries.Add(1)
		http.Error(w, "bad token secret-token", http.StatusUnauthorized)
	}))
	defer s.Close()

	c := testClient(t, s)
	err := c.Send(context.Background(), Event{Type: EventWarning})
	if err == nil {
		t.Fatal("want an error")
	}
	if tries.Load() != 1 {
		t.Errorf("tries = %d, want 1: a bad token does not fix itself", tries.Load())
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("the token must never be in an error: %v", err)
	}
}

func TestSendStopsWhenTheContextEnds(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer s.Close()
	old := retryWaits
	retryWaits = []time.Duration{time.Hour}
	defer func() { retryWaits = old }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := testClient(t, s)
	if err := c.Send(ctx, Event{Type: EventWarning}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/control/ -count=1`
Expected: FAIL — the package does not build (`New` undefined).

- [ ] **Step 3: Implement**

Create `internal/control/control.go`:

```go
// Package control talks to an optional control server, for example Sentinel.
// It sends events and reads a short list of commands. Only the updater uses
// it: the gate has no network and opens no port, so the server can never
// connect to Bosun.
package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mehrad-meraji/bosun/internal/gate"
)

// Version is Bosun's version. It goes out with every event. Bump it by hand.
const Version = "0.2.0"

// Schema is the event format version. Bump it when a field changes meaning.
const Schema = 1

// Event types.
const (
	EventUpdateDone       = "update.done"
	EventUpdateRolledBack = "update.rolled_back"
	EventUpdateFailed     = "update.failed"
	EventVersionAvailable  = "version.available"
	EventGateRefused      = "gate.refused"
	EventRegistryFailing  = "registry.failing"
	EventRecovery         = "recovery"
	EventWarning          = "warning"
	EventCommandResult    = "command.result"
)

// Backup is the size and time of the backup an update took.
type Backup struct {
	Bytes int64 `json:"bytes"`
	Ms    int64 `json:"ms"`
}

// Event is one thing that happened. The server drops repeats by ID. The
// body never holds registry logins, notify URLs or env vars.
type Event struct {
	Schema     int         `json:"schema"`
	ID         string      `json:"id"`
	Time       time.Time   `json:"time"`
	Host       string      `json:"host"`
	Version    string      `json:"bosun_version"`
	Type       string      `json:"type"`
	Container  string      `json:"container,omitempty"`
	Image      string      `json:"image,omitempty"`
	FromDigest string      `json:"from_digest,omitempty"`
	ToDigest   string      `json:"to_digest,omitempty"`
	DowntimeMs int64       `json:"downtime_ms,omitempty"`
	Backup     *Backup     `json:"backup,omitempty"`
	Steps      []gate.Step `json:"steps,omitempty"`
	Reason     string      `json:"reason,omitempty"`
	CommandID  string      `json:"command_id,omitempty"`
}

// Client sends events and reads commands. A nil Client means the link is off.
type Client struct {
	url   string // base URL, no trailing slash
	token string
	host  string
	http  *http.Client

	seen map[string]time.Time // command IDs already run; see commands.go
}

// New returns nil when rawURL is empty: the link is off. The token comes
// from a file, like the notify URLs, so it is not in the env.
func New(rawURL, tokenFile, host string, insecure bool) (*Client, error) {
	if rawURL == "" {
		return nil, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("BOSUN_CONTROL_URL %q: %w", rawURL, err)
	}
	switch {
	case u.Host == "":
		return nil, fmt.Errorf("BOSUN_CONTROL_URL %q has no host; use https://your-server", rawURL)
	case u.Scheme == "https":
	case u.Scheme == "http" && insecure:
	default:
		return nil, fmt.Errorf("BOSUN_CONTROL_URL %q must start with https://. For a server on your own LAN, set BOSUN_CONTROL_INSECURE=true", rawURL)
	}
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read the control token from %s: %w. Mount the file under /etc/bosun, or unset BOSUN_CONTROL_URL", tokenFile, err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return nil, fmt.Errorf("the control token file %s is empty; put the server's token in it", tokenFile)
	}
	if host == "" {
		return nil, fmt.Errorf("the control link needs a host name, and Docker gave none; set BOSUN_HOST on bosun-gate")
	}
	return &Client{
		url:   strings.TrimSuffix(rawURL, "/"),
		token: token,
		host:  host,
		http:  &http.Client{Timeout: 20 * time.Second},
		seen:  map[string]time.Time{},
	}, nil
}

// Host is the name this Bosun reports as.
func (c *Client) Host() string { return c.host }

// retryWaits are the waits between tries. A var, so tests can shorten them.
var retryWaits = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute}

// statusError is a reply with a status of 300 or more. The reply body is
// never kept: a server could echo the token back in it.
type statusError struct{ status int }

func (e *statusError) Error() string {
	return fmt.Sprintf("control server: %d %s", e.status, http.StatusText(e.status))
}

// Send posts one event. It tries up to four times: at once, then after 5 s,
// 30 s and 2 min. A bad request or a bad token is not tried again, because
// it will not fix itself. The caller logs and drops what comes back.
func (c *Client) Send(ctx context.Context, ev Event) error {
	ev.Schema, ev.Version = Schema, Version
	if ev.ID == "" {
		ev.ID = newID()
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	if ev.Host == "" {
		ev.Host = c.host
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	for i := 0; ; i++ {
		err = c.post(ctx, "/events", b)
		if err == nil {
			return nil
		}
		var se *statusError
		if errors.As(err, &se) && se.status < 500 && se.status != http.StatusTooManyRequests {
			return err
		}
		if i >= len(retryWaits) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryWaits[i]):
		}
	}
}

func (c *Client) post(ctx context.Context, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("control server: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 300 {
		return &statusError{status: resp.StatusCode}
	}
	return nil
}

// newID makes a random UUID for one event.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano()) // ponytail: crypto/rand does not fail in practice; a clock id still lets the server drop repeats
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
```

A `net/http` request never puts the `Authorization` header in an error, so `post`'s errors carry the URL at worst, never the token.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/control/ -count=1 -v`
Expected: PASS, all seven tests.

- [ ] **Step 5: Commit**

```bash
git add internal/control
git commit -m "Control link: the event type and the sender"
```

---

### Task 5: `internal/control` — the command reader

**Files:**
- Create: `internal/control/commands.go`
- Test: `internal/control/commands_test.go`
- Test (fuzz): the same file

**Interfaces:**
- Consumes: `Client` (Task 4).
- Produces:
  - `control.Command{ID, Type, Container string}` (json `id`, `type`, `container,omitempty`)
  - `control.Refusal{ID, Reason string}`
  - `control.CmdCheck = "check"`, `control.CmdSkipClear = "skip_clear"`
  - `(*Client).Commands(ctx context.Context) ([]Command, []Refusal, error)` — good commands, and the ones to report as `refused`
  - unexported `parse(b []byte) ([]Command, []Refusal, error)` for the fuzz test

- [ ] **Step 1: Write the failing tests**

Create `internal/control/commands_test.go`:

```go
package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseCommands(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		wantOK     []string // command IDs kept
		wantRefuse []string // command IDs refused
		wantErr    bool
	}{
		{name: "both commands", body: `[{"id":"a","type":"check"},{"id":"b","type":"skip_clear","container":"nginx"}]`, wantOK: []string{"a", "b"}},
		{name: "empty list", body: `[]`},
		{name: "unknown command", body: `[{"id":"a","type":"reboot"}]`, wantRefuse: []string{"a"}},
		{name: "unknown field", body: `[{"id":"a","type":"check","image":"evil"}]`, wantErr: true},
		{name: "skip_clear without a container", body: `[{"id":"a","type":"skip_clear"}]`, wantRefuse: []string{"a"}},
		{name: "skip_clear with a bad container", body: `[{"id":"a","type":"skip_clear","container":"../etc"}]`, wantRefuse: []string{"a"}},
		{name: "check with a container", body: `[{"id":"a","type":"check","container":"nginx"}]`, wantRefuse: []string{"a"}},
		{name: "no id", body: `[{"type":"check"}]`},
		{name: "bad id", body: `[{"id":"a b","type":"check"}]`},
		{name: "not a list", body: `{"id":"a","type":"check"}`, wantErr: true},
		{name: "junk", body: `not json`, wantErr: true},
		{name: "two objects", body: `[] []`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmds, refs, err := parse([]byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			var ok, refused []string
			for _, c := range cmds {
				ok = append(ok, c.ID)
			}
			for _, r := range refs {
				refused = append(refused, r.ID)
			}
			if strings.Join(ok, ",") != strings.Join(tc.wantOK, ",") {
				t.Errorf("kept = %v, want %v", ok, tc.wantOK)
			}
			if strings.Join(refused, ",") != strings.Join(tc.wantRefuse, ",") {
				t.Errorf("refused = %v, want %v", refused, tc.wantRefuse)
			}
			for _, r := range refs {
				if r.Reason == "" {
					t.Error("a refusal must say why")
				}
			}
		})
	}
}

func TestParseRefusesATooLongList(t *testing.T) {
	var cmds []Command
	for i := 0; i < maxCommands+1; i++ {
		cmds = append(cmds, Command{ID: string(rune('a'+i%26)) + string(rune('a'+i/26)), Type: CmdCheck})
	}
	b, _ := json.Marshal(cmds)
	if _, _, err := parse(b); err == nil {
		t.Error("want an error for a list over the cap")
	}
}

func TestCommandsDropsRepeats(t *testing.T) {
	body := `[{"id":"a","type":"check"}]`
	var host string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/commands" {
			t.Errorf("got %s %s, want GET /commands", r.Method, r.URL.Path)
		}
		host = r.URL.Query().Get("host")
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Errorf("auth = %q", got)
		}
		io.WriteString(w, body)
	}))
	defer s.Close()

	c := testClient(t, s)
	cmds, _, err := c.Commands(context.Background())
	if err != nil || len(cmds) != 1 {
		t.Fatalf("first poll = %v, %v, want one command", cmds, err)
	}
	if host != "worker-1" {
		t.Errorf("host = %q, want worker-1", host)
	}
	cmds, refs, err := c.Commands(context.Background())
	if err != nil || len(cmds) != 0 || len(refs) != 0 {
		t.Errorf("second poll = %v, %v, %v, want the repeat dropped and not reported", cmds, refs, err)
	}
}

func TestCommandsForgetsOldIDs(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"a","type":"check"}]`)
	}))
	defer s.Close()
	c := testClient(t, s)
	if _, _, err := c.Commands(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.seen["a"] = time.Now().Add(-25 * time.Hour) // older than the window
	cmds, _, err := c.Commands(context.Background())
	if err != nil || len(cmds) != 1 {
		t.Errorf("after 25 hours = %v, %v, want the command again", cmds, err)
	}
}

func TestCommandsFailsOnABadStatus(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no secret-token for you", http.StatusUnauthorized)
	}))
	defer s.Close()
	_, _, err := testClient(t, s).Commands(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("the token must never be in an error: %v", err)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(`[{"id":"a","type":"check"}]`))
	f.Add([]byte(`[{"id":"a","type":"skip_clear","container":"nginx"}]`))
	f.Add([]byte(`[{"id":"a","type":"skip_clear","container":"../../etc/passwd"}]`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, b []byte) {
		cmds, refs, err := parse(b)
		if err != nil {
			if cmds != nil || refs != nil {
				t.Error("an error must take nothing with it")
			}
			return
		}
		for _, c := range cmds {
			if c.Type != CmdCheck && c.Type != CmdSkipClear {
				t.Errorf("kept an unknown command %q", c.Type)
			}
			if !idRE.MatchString(c.ID) {
				t.Errorf("kept a bad id %q", c.ID)
			}
			if c.Type == CmdSkipClear && !nameRE.MatchString(c.Container) {
				t.Errorf("kept a bad container %q", c.Container)
			}
			if c.Type == CmdCheck && c.Container != "" {
				t.Errorf("kept a check with a container %q", c.Container)
			}
		}
	})
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/control/ -run 'Parse|Commands' -count=1`
Expected: FAIL — `parse` undefined.

- [ ] **Step 3: Implement**

Create `internal/control/commands.go`:

```go
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// The only two commands there are. On purpose: a hacked server can make
// Bosun run an early round and retry a version it already refused. It
// cannot pick an image, roll back, or touch data.
const (
	CmdCheck     = "check"
	CmdSkipClear = "skip_clear"
)

// Command is one thing the server asks for.
type Command struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Container string `json:"container,omitempty"`
}

// Refusal is a command Bosun will not run, to report back.
type Refusal struct {
	ID     string
	Reason string
}

const (
	maxCommands = 100     // more than this in one reply is not a real list
	maxBody     = 64 << 10
	replayFor   = 24 * time.Hour
	maxSeen     = 10000 // ponytail: a flat cap; if a server ever floods unique IDs, the map is cleared and a replay costs at most one extra round
)

var (
	idRE   = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,128}$`)
	nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`) // same as the gate's; the gate checks again
)

// Commands asks the server what to do. It returns the commands to run and
// the ones to report as refused. A command already run in the last 24 hours
// is dropped without a word, so a replay cannot run it twice.
func (c *Client) Commands(ctx context.Context) ([]Command, []Refusal, error) {
	u := c.url + "/commands?" + url.Values{"host": {c.host}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("control server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, nil, &statusError{status: resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, nil, fmt.Errorf("read the command list: %w", err)
	}
	cmds, refs, err := parse(b)
	if err != nil {
		return nil, nil, err
	}
	return c.fresh(cmds), refs, nil
}

// parse reads the command list. Unknown fields and unknown commands are
// refused, never guessed at.
func parse(b []byte) ([]Command, []Refusal, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var list []Command
	if err := dec.Decode(&list); err != nil {
		return nil, nil, fmt.Errorf("bad command list: %w", err)
	}
	if dec.More() {
		return nil, nil, fmt.Errorf("bad command list: more than one list")
	}
	if len(list) > maxCommands {
		return nil, nil, fmt.Errorf("bad command list: %d commands, more than the %d allowed", len(list), maxCommands)
	}
	var cmds []Command
	var refs []Refusal
	for _, cmd := range list {
		if !idRE.MatchString(cmd.ID) {
			// Without a usable id there is nothing to report it against.
			log.Printf("control server: dropped a command with a bad id")
			continue
		}
		switch {
		case cmd.Type == CmdCheck && cmd.Container == "":
			cmds = append(cmds, cmd)
		case cmd.Type == CmdCheck:
			refs = append(refs, Refusal{ID: cmd.ID, Reason: "check takes no container"})
		case cmd.Type == CmdSkipClear && nameRE.MatchString(cmd.Container):
			cmds = append(cmds, cmd)
		case cmd.Type == CmdSkipClear:
			refs = append(refs, Refusal{ID: cmd.ID, Reason: "skip_clear needs a container name"})
		default:
			refs = append(refs, Refusal{ID: cmd.ID, Reason: fmt.Sprintf("unknown command %q", cmd.Type)})
		}
	}
	return cmds, refs, nil
}

// fresh drops commands already run in the last 24 hours.
func (c *Client) fresh(in []Command) []Command {
	now := time.Now()
	for id, t := range c.seen {
		if now.Sub(t) > replayFor {
			delete(c.seen, id)
		}
	}
	if len(c.seen) > maxSeen {
		log.Printf("control server: over %d command ids in a day; forgetting them", maxSeen)
		c.seen = map[string]time.Time{}
	}
	var out []Command
	for _, cmd := range in {
		if _, ok := c.seen[cmd.ID]; ok {
			log.Printf("control server: command %s was already run; dropped", cmd.ID)
			continue
		}
		c.seen[cmd.ID] = now
		out = append(out, cmd)
	}
	return out
}
```

Import `bytes` in `commands.go` for that reader.

`Commands` is called from one goroutine (the poll loop), so `seen` needs no lock. Write that as a comment on the `seen` field in `control.go`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/control/ -count=1`
Expected: PASS.

- [ ] **Step 5: Run the fuzz test briefly**

Run: `go test ./internal/control/ -run FuzzParse -fuzz FuzzParse -fuzztime 30s`
Expected: no failures. Then `rm -rf internal/control/testdata/fuzz` only if it is empty; keep any crasher the fuzzer found and fix the code instead.

- [ ] **Step 6: Commit**

```bash
git add internal/control
git commit -m "Control link: the command reader, with a replay guard"
```

---

### Task 6: the event builders in the updater

Pure funcs, no network, no goroutines: they turn one round outcome into one `control.Event`. A reviewer can check the mapping here without reading the wiring.

**Files:**
- Create: `internal/updater/events.go`
- Test: `internal/updater/events_test.go`
- Modify: `internal/control/control.go` (add the `Status` field the `command.result` event needs)

**Interfaces:**
- Consumes: `control.Event`, the `Event*` type constants, `control.Backup` (Task 4); `gate.Watched`, `gate.Result`, `gate.Step`, `gate.StatusReverted`, `gate.IsRefused` (Tasks 1 and 3); `state.Event`.
- Produces (all unexported, all used by Task 7 and Task 8):
  - `updateEvent(w gate.Watched, digest string, res gate.Result, cmdID string) control.Event`
  - `failedEvent(w gate.Watched, digest string, err error, cmdID string) control.Event`
  - `availableEvent(w gate.Watched, digest, cmdID string) control.Event`
  - `registryEvent(w gate.Watched, err error, cmdID string) control.Event`
  - `gateEvent(e state.Event, cmdID string) control.Event`
  - `resultEvent(cmdID, status, detail string) control.Event`

- [ ] **Step 1: Add the `Status` field**

In `internal/control/control.go`, in `Event`, after `Reason`:

```go
	Status     string      `json:"status,omitempty"` // command.result only: done, busy, refused or failed
```

- [ ] **Step 2: Write the failing tests**

Create `internal/updater/events_test.go`:

```go
package updater

import (
	"errors"
	"testing"
	"time"

	"github.com/mehrad-meraji/bosun/internal/control"
	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/state"
)

var w1 = gate.Watched{Name: "redis", Ref: "redis:7.4", Digests: []string{"sha256:old"}, Mode: "update"}

func TestUpdateEventDone(t *testing.T) {
	res := gate.Result{
		Status: gate.StatusDone, Downtime: 38 * time.Second,
		Backup: 2 * time.Second, BackupBytes: 1024,
		Steps: []gate.Step{{Name: "pull", Status: "ok"}},
	}
	ev := updateEvent(w1, "sha256:new", res, "")
	if ev.Type != control.EventUpdateDone || ev.Container != "redis" || ev.Image != "redis:7.4" {
		t.Errorf("event = %+v", ev)
	}
	if ev.FromDigest != "sha256:old" || ev.ToDigest != "sha256:new" || ev.DowntimeMs != 38000 {
		t.Errorf("digests/downtime = %+v", ev)
	}
	if ev.Backup == nil || ev.Backup.Bytes != 1024 || ev.Backup.Ms != 2000 {
		t.Errorf("backup = %+v", ev.Backup)
	}
	if len(ev.Steps) != 1 || ev.CommandID != "" {
		t.Errorf("steps/command = %+v", ev)
	}
}

func TestUpdateEventRolledBack(t *testing.T) {
	ev := updateEvent(w1, "sha256:new", gate.Result{Status: gate.StatusReverted}, "cmd-1")
	if ev.Type != control.EventUpdateRolledBack || ev.Reason == "" || ev.CommandID != "cmd-1" {
		t.Errorf("event = %+v", ev)
	}
	if ev.Backup != nil {
		t.Errorf("backup must be absent when there was none: %+v", ev.Backup)
	}
}

func TestFailedEventAndRefusedEvent(t *testing.T) {
	ev := failedEvent(w1, "sha256:new", errors.New("pull: no such tag"), "")
	if ev.Type != control.EventUpdateFailed || ev.Reason != "pull: no such tag" {
		t.Errorf("event = %+v", ev)
	}
	if len(ev.Steps) != 0 {
		t.Errorf("update.failed carries no steps; the gate sends steps only with a result: %+v", ev.Steps)
	}
	ev = failedEvent(w1, "sha256:new", &gate.RefusedError{Msg: "redis is notify-only"}, "")
	if ev.Type != control.EventGateRefused {
		t.Errorf("a gate refusal is its own event type: %+v", ev)
	}
}

func TestAvailableAndRegistryEvents(t *testing.T) {
	ev := availableEvent(w1, "sha256:new", "")
	if ev.Type != control.EventVersionAvailable || ev.ToDigest != "sha256:new" {
		t.Errorf("event = %+v", ev)
	}
	ev = registryEvent(w1, errors.New("429 too many requests"), "")
	if ev.Type != control.EventRegistryFailing || ev.Reason == "" || ev.Container != "redis" {
		t.Errorf("event = %+v", ev)
	}
}

func TestGateEvent(t *testing.T) {
	when := time.Date(2026, 9, 11, 4, 0, 0, 0, time.UTC)
	ev := gateEvent(state.Event{Time: when, Kind: "recovered", Name: "app", Message: "app: the old version is back"}, "")
	if ev.Type != control.EventRecovery || !ev.Time.Equal(when) || ev.Container != "app" || ev.Reason == "" {
		t.Errorf("event = %+v", ev)
	}
	ev = gateEvent(state.Event{Kind: "something-new", Name: "app", Message: "hm"}, "")
	if ev.Type != control.EventWarning {
		t.Errorf("an unknown gate kind is a warning: %+v", ev)
	}
}

func TestResultEvent(t *testing.T) {
	ev := resultEvent("cmd-1", "busy", "a round is running")
	if ev.Type != control.EventCommandResult || ev.CommandID != "cmd-1" || ev.Status != "busy" || ev.Reason == "" {
		t.Errorf("event = %+v", ev)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/updater/ -run 'Event' -count=1`
Expected: FAIL — `updateEvent` undefined.

- [ ] **Step 4: Implement**

Create `internal/updater/events.go`:

```go
package updater

import (
	"github.com/mehrad-meraji/bosun/internal/control"
	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/state"
)

// first is the digest a container runs now, or "" if Docker gave none.
func first(ds []string) string {
	if len(ds) == 0 {
		return ""
	}
	return ds[0]
}

// updateEvent reports a finished update, done or rolled back.
func updateEvent(w gate.Watched, digest string, res gate.Result, cmdID string) control.Event {
	ev := control.Event{
		Type:       control.EventUpdateDone,
		Container:  w.Name,
		Image:      w.Ref,
		FromDigest: first(w.Digests),
		ToDigest:   digest,
		DowntimeMs: res.Downtime.Milliseconds(),
		Steps:      res.Steps,
		CommandID:  cmdID,
	}
	if res.Status == gate.StatusReverted {
		ev.Type = control.EventUpdateRolledBack
		ev.Reason = "the new version did not come up healthy"
	}
	if res.BackupBytes > 0 {
		ev.Backup = &control.Backup{Bytes: res.BackupBytes, Ms: res.Backup.Milliseconds()}
	}
	return ev
}

// failedEvent reports an update that did not finish. The gate sends steps
// only with a result, so there are none here; the reason says what failed.
func failedEvent(w gate.Watched, digest string, err error, cmdID string) control.Event {
	t := control.EventUpdateFailed
	if gate.IsRefused(err) {
		t = control.EventGateRefused
	}
	return control.Event{
		Type:       t,
		Container:  w.Name,
		Image:      w.Ref,
		FromDigest: first(w.Digests),
		ToDigest:   digest,
		Reason:     err.Error(),
		CommandID:  cmdID,
	}
}

// availableEvent reports a new version for a notify-only container.
func availableEvent(w gate.Watched, digest, cmdID string) control.Event {
	return control.Event{
		Type:       control.EventVersionAvailable,
		Container:  w.Name,
		Image:      w.Ref,
		FromDigest: first(w.Digests),
		ToDigest:   digest,
		CommandID:  cmdID,
	}
}

// registryEvent reports a registry that keeps failing.
func registryEvent(w gate.Watched, err error, cmdID string) control.Event {
	return control.Event{
		Type:      control.EventRegistryFailing,
		Container: w.Name,
		Image:     w.Ref,
		Reason:    err.Error(),
		CommandID: cmdID,
	}
}

// gateEvent passes on something that happened in the gate while no call was
// open, such as crash recovery.
func gateEvent(e state.Event, cmdID string) control.Event {
	t := control.EventWarning
	if e.Kind == "recovered" {
		t = control.EventRecovery
	}
	return control.Event{
		Type:      t,
		Time:      e.Time,
		Container: e.Name,
		Reason:    e.Message,
		CommandID: cmdID,
	}
}

// resultEvent tells the server what became of one of its commands, so it can
// take the command off its list.
func resultEvent(cmdID, status, detail string) control.Event {
	return control.Event{
		Type:      control.EventCommandResult,
		Status:    status,
		Reason:    detail,
		CommandID: cmdID,
	}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/updater/ ./internal/control/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/updater/events.go internal/updater/events_test.go internal/control
git commit -m "Control link: build events from a round's outcome"
```

---

### Task 7: send the events

**Files:**
- Modify: `internal/updater/updater.go` (`Updater` fields, `StartEvents`, `emit`, `Round` → `round`)
- Test: `internal/updater/updater_test.go`

**Interfaces:**
- Consumes: Task 6's builders; `control.Client`, `(*control.Client).Send`.
- Produces:
  - `Updater.Control *control.Client` (nil means the link is off)
  - `(*Updater).StartEvents(ctx context.Context)`
  - `(*Updater).emit(ev control.Event)` (unexported)
  - `(*Updater).round(ctx context.Context, dryRun bool, cmdID string) ([]string, error)`; `Round(ctx, dryRun)` stays and calls `round(ctx, dryRun, "")`
  - `Updater.Gate` interface gains nothing in this task.

- [ ] **Step 1: Write the failing tests**

Add to `internal/updater/updater_test.go`:

```go
// controlFake is a control server that records the events it is sent.
func controlFake(t *testing.T) (*control.Client, func() []control.Event) {
	t.Helper()
	var mu sync.Mutex
	var got []control.Event
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev control.Event
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ev); err != nil {
			t.Error(err)
		}
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}))
	t.Cleanup(s.Close)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := control.New(s.URL, tok, "worker-1", true)
	if err != nil {
		t.Fatal(err)
	}
	// events go out in another goroutine; wait for n of them
	return c, func() []control.Event {
		for i := 0; i < 100; i++ {
			mu.Lock()
			n := len(got)
			mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		return append([]control.Event(nil), got...)
	}
}

func TestRoundSendsAnEvent(t *testing.T) {
	u, _, _ := setup(web("update", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	c, events := controlFake(t)
	u.Control = c
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u.StartEvents(ctx)

	if _, err := u.Round(ctx, false); err != nil {
		t.Fatal(err)
	}
	evs := events()
	if len(evs) != 1 || evs[0].Type != control.EventUpdateDone || evs[0].Container != "web" {
		t.Fatalf("events = %+v, want one update.done for web", evs)
	}
	if evs[0].Host != "worker-1" || evs[0].Schema != control.Schema {
		t.Errorf("event = %+v, want the fixed fields filled in", evs[0])
	}
}

func TestDryRunSendsNothing(t *testing.T) {
	u, _, _ := setup(web("update", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	c, events := controlFake(t)
	u.Control = c
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u.StartEvents(ctx)
	if _, err := u.Round(ctx, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if evs := events(); len(evs) != 0 {
		t.Errorf("a dry run must send nothing: %+v", evs)
	}
}

func TestRoundWorksWithTheLinkOff(t *testing.T) {
	u, g, notes := setup(web("update", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	u.StartEvents(context.Background()) // no Control: must not panic and must start nothing
	if _, err := u.Round(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(g.updates) != 1 || len(*notes) != 1 {
		t.Errorf("the round must work the same with the link off: %v %v", g.updates, *notes)
	}
}

func TestEmitDropsWhenTheQueueIsFull(t *testing.T) {
	u := &Updater{Control: &control.Client{}} // never read: emit only fills the queue
	u.evs = make(chan control.Event, 1)
	u.emit(control.Event{Type: control.EventWarning})
	u.emit(control.Event{Type: control.EventWarning}) // must not block
	if len(u.evs) != 1 {
		t.Errorf("queue = %d, want the second event dropped", len(u.evs))
	}
}

func TestGateEventsGoOutAsEvents(t *testing.T) {
	u, g, _ := setup(nil, fakeReg{digest: "sha256:new"})
	g.events = []state.Event{{Kind: "recovered", Name: "app", Message: "app: the old version is back"}}
	c, events := controlFake(t)
	u.Control = c
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u.StartEvents(ctx)
	if _, err := u.Round(ctx, false); err != nil {
		t.Fatal(err)
	}
	evs := events()
	if len(evs) != 1 || evs[0].Type != control.EventRecovery {
		t.Fatalf("events = %+v, want one recovery event", evs)
	}
}
```

Add the imports the tests need: `encoding/json`, `io`, `net/http`, `net/http/httptest`, `path/filepath`, `sync`, `time`, and `github.com/mehrad-meraji/bosun/internal/control`.

`control.Client{}` in `TestEmitDropsWhenTheQueueIsFull` is a bare struct on purpose: `emit` must only look at whether it is nil.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/updater/ -count=1`
Expected: FAIL — `u.Control` and `u.evs` undefined.

- [ ] **Step 3: Implement**

In `internal/updater/updater.go`, add to `Updater`:

```go
	// Control is the control server link, or nil when it is off.
	Control *control.Client

	evs chan control.Event // events waiting to go out
```

and after `ErrBusy`:

```go
// eventQueue is how many events wait to go out. A slow server must never
// hold up an update, so a full queue drops the oldest news: the log keeps it.
const eventQueue = 100

// StartEvents starts the one goroutine that sends events. It does nothing
// when the link is off. Call it once, before Run.
func (u *Updater) StartEvents(ctx context.Context) {
	if u.Control == nil {
		return
	}
	u.evs = make(chan control.Event, eventQueue)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-u.evs:
				if err := u.Control.Send(ctx, ev); err != nil && ctx.Err() == nil {
					log.Printf("control event %s dropped: %v", ev.Type, err)
				}
			}
		}
	}()
}

// emit queues one event. It never blocks and never fails an update.
func (u *Updater) emit(ev control.Event) {
	if u.Control == nil || u.evs == nil {
		return
	}
	select {
	case u.evs <- ev:
	default:
		log.Printf("control events are backed up; dropped a %s event", ev.Type)
	}
}
```

Rename `Round` to `round` with the extra parameter and keep `Round` as the plain entry point:

```go
// Round checks every watched container once. dryRun only reports.
func (u *Updater) Round(ctx context.Context, dryRun bool) ([]string, error) {
	return u.round(ctx, dryRun, "")
}

// round is Round. cmdID is set when a control-server command asked for it,
// so the events say which command they belong to.
func (u *Updater) round(ctx context.Context, dryRun bool, cmdID string) ([]string, error) {
```

In `round`, add one `u.emit(...)` next to each outcome. Nothing else in the loop changes:

```go
		d, err := u.Reg.Digest(ctx, w.Ref)
		if err != nil {
			add("%s: registry check failed: %v", w.Name, err)
			if !dryRun {
				if u.fails[w.Name]++; u.fails[w.Name] == 3 {
					u.Notify(fmt.Sprintf("%s: registry check failed 3 rounds in a row: %v", w.Name, err))
					u.emit(registryEvent(w, err, cmdID))
				}
			}
			continue
		}
```

```go
		case w.Mode == "notify":
			msg := add("%s: a new version of %s is ready (notify only)", w.Name, w.Ref)
			if !dryRun && u.told[w.Name] != d {
				u.told[w.Name] = d
				u.Notify(msg)
				u.emit(availableEvent(w, d, cmdID))
			}
```

```go
			auth, err := u.Reg.Auth(w.Ref)
			if err != nil {
				u.Notify(add("%s: could not read the registry login: %v", w.Name, err))
				u.emit(failedEvent(w, d, err, cmdID))
				continue
			}
			res, err := u.Gate.Update(ctx, w.Name, d, auth)
			if err != nil {
				u.Notify(add("%s: update failed: %v", w.Name, err))
				u.emit(failedEvent(w, d, err, cmdID))
				continue
			}
			u.Notify(add("%s", res.Message))
			u.emit(updateEvent(w, d, res, cmdID))
```

and give `sendEvents` the command id:

```go
	if !dryRun {
		u.sendEvents(ctx, cmdID)
	}
```

```go
func (u *Updater) sendEvents(ctx context.Context, cmdID string) {
	evs, err := u.Gate.Events(ctx)
	if err != nil {
		log.Printf("read gate events: %v", err)
		return
	}
	for _, e := range evs {
		u.Notify(e.Message)
		u.emit(gateEvent(e, cmdID))
	}
}
```

`Run`'s call becomes `u.sendEvents(ctx, "")`, and its round call becomes `u.round(ctx, false, "")`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/updater/ -count=1 -race`
Expected: PASS, no race.

- [ ] **Step 5: Commit**

```bash
git add internal/updater
git commit -m "Control link: queue and send events from a round"
```

---

### Task 8: read and run the commands

**Files:**
- Create: `internal/updater/commands.go`
- Test: `internal/updater/commands_test.go`
- Modify: `internal/updater/updater.go` (the `Gate` interface gains `SkipClear`)
- Modify: `internal/updater/updater_test.go` (`fakeGate` gains `SkipClear`)

**Interfaces:**
- Consumes: `(*control.Client).Commands`, `control.Command`, `control.Refusal`, `control.CmdCheck`, `control.CmdSkipClear`, `resultEvent`, `(*Updater).round`, `(*gate.Client).SkipClear`, `gate.SkipClearResult`, `gate.IsRefused`, `state.ErrBusy`.
- Produces:
  - `Gate` interface gains `SkipClear(ctx context.Context, name string) (gate.SkipClearResult, error)`
  - `(*Updater).Commands(ctx context.Context, poll time.Duration) error`
  - `(*Updater).runCommand(ctx context.Context, c control.Command) (status, detail string)` (unexported)

- [ ] **Step 1: Write the failing tests**

Create `internal/updater/commands_test.go`:

```go
package updater

import (
	"context"
	"errors"
	"testing"

	"github.com/mehrad-meraji/bosun/internal/control"
	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/state"
)

func TestRunCommandCheck(t *testing.T) {
	u, g, _ := setup(web("update", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	status, _ := u.runCommand(context.Background(), control.Command{ID: "c1", Type: control.CmdCheck})
	if status != "done" || len(g.updates) != 1 {
		t.Errorf("status = %q, updates = %v, want done and one update", status, g.updates)
	}
}

func TestRunCommandCheckIsBusy(t *testing.T) {
	u, _, _ := setup(nil, fakeReg{digest: "sha256:new"})
	u.mu.Lock() // a round is running
	defer u.mu.Unlock()
	status, detail := u.runCommand(context.Background(), control.Command{ID: "c1", Type: control.CmdCheck})
	if status != "busy" || detail == "" {
		t.Errorf("status = %q, %q, want busy with a reason", status, detail)
	}
}

func TestRunCommandSkipClear(t *testing.T) {
	u, g, _ := setup(nil, fakeReg{})
	g.cleared = gate.SkipClearResult{Cleared: 2, Message: "app: skip list cleared (2)"}
	status, detail := u.runCommand(context.Background(), control.Command{ID: "c1", Type: control.CmdSkipClear, Container: "app"})
	if status != "done" || detail == "" || g.skipCleared != "app" {
		t.Errorf("status = %q, %q, cleared %q, want done for app", status, detail, g.skipCleared)
	}
}

func TestRunCommandSkipClearBusyAndRefused(t *testing.T) {
	u, g, _ := setup(nil, fakeReg{})
	g.clearErr = state.ErrBusy // the gate's own error; over the socket it is a 409, which gate.IsBusy also reads
	if status, _ := u.runCommand(context.Background(), control.Command{ID: "c1", Type: control.CmdSkipClear, Container: "app"}); status != "busy" {
		t.Errorf("status = %q, want busy", status)
	}
	g.clearErr = &gate.RefusedError{Msg: "bad container name"}
	if status, _ := u.runCommand(context.Background(), control.Command{ID: "c2", Type: control.CmdSkipClear, Container: "app"}); status != "refused" {
		t.Errorf("status = %q, want refused", status)
	}
	g.clearErr = errors.New("disk on fire")
	if status, _ := u.runCommand(context.Background(), control.Command{ID: "c3", Type: control.CmdSkipClear, Container: "app"}); status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
}

func TestRunCommandUnknownType(t *testing.T) {
	u, _, _ := setup(nil, fakeReg{})
	if status, _ := u.runCommand(context.Background(), control.Command{ID: "c1", Type: "reboot"}); status != "refused" {
		t.Errorf("status = %q, want refused", status)
	}
}
```

Add to `fakeGate` in `internal/updater/updater_test.go`:

```go
	cleared     gate.SkipClearResult
	clearErr    error
	skipCleared string
```

```go
func (f *fakeGate) SkipClear(_ context.Context, name string) (gate.SkipClearResult, error) {
	f.skipCleared = name
	return f.cleared, f.clearErr
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/updater/ -run RunCommand -count=1`
Expected: FAIL — `runCommand` undefined.

- [ ] **Step 3: Implement**

Add `SkipClear` to the `Gate` interface in `internal/updater/updater.go`:

```go
type Gate interface {
	List(ctx context.Context) ([]gate.Watched, error)
	Update(ctx context.Context, name, digest, auth string) (gate.Result, error)
	Events(ctx context.Context) ([]state.Event, error)
	SkipClear(ctx context.Context, name string) (gate.SkipClearResult, error)
}
```

Create `internal/updater/commands.go`:

```go
package updater

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/mehrad-meraji/bosun/internal/control"
	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/state"
)

// Commands asks the control server for work every poll until ctx ends. The
// server never connects to Bosun, so a command runs up to one poll late.
// That is the price of having no open port.
func (u *Updater) Commands(ctx context.Context, poll time.Duration) error {
	if u.Control == nil {
		return nil
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		cmds, refs, err := u.Control.Commands(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("read commands: %v", err) // try again at the next poll
			}
			continue
		}
		for _, r := range refs {
			log.Printf("refused command %s: %s", r.ID, r.Reason)
			u.emit(resultEvent(r.ID, "refused", r.Reason))
		}
		for _, c := range cmds {
			status, detail := u.runCommand(ctx, c)
			log.Printf("command %s (%s): %s: %s", c.ID, c.Type, status, detail)
			u.emit(resultEvent(c.ID, status, detail))
		}
	}
}

// runCommand does one command and says how it went: done, busy, refused or
// failed. The server uses that to take the command off its list.
func (u *Updater) runCommand(ctx context.Context, c control.Command) (string, string) {
	switch c.Type {
	case control.CmdCheck:
		lines, err := u.round(ctx, false, c.ID)
		switch {
		case errors.Is(err, ErrBusy):
			return "busy", err.Error()
		case err != nil:
			return "failed", err.Error()
		}
		return "done", fmt.Sprintf("checked %d containers", len(lines))
	case control.CmdSkipClear:
		res, err := u.Gate.SkipClear(ctx, c.Container)
		switch {
		case gate.IsBusy(err):
			return "busy", err.Error()
		case gate.IsRefused(err):
			return "refused", err.Error()
		case err != nil:
			return "failed", err.Error()
		}
		return "done", res.Message
	}
	return "refused", "unknown command " + c.Type
}
```

Add `"fmt"` to the imports, and drop `state` from them if nothing else in the file uses it: `gate.IsBusy` (Task 1) covers both the direct error and the gate's 409.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/updater/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Wire it up in `main.go`**

Add to the settings block:

```go
	controlURL      = os.Getenv("BOSUN_CONTROL_URL")
	controlToken    = filepath.Clean(env("BOSUN_CONTROL_TOKEN_FILE", "/etc/bosun/control-token"))
	controlCommands = os.Getenv("BOSUN_CONTROL_COMMANDS") == "true"
	controlPoll     = env("BOSUN_CONTROL_POLL", "60s")
	controlInsecure = os.Getenv("BOSUN_CONTROL_INSECURE") == "true"
)

// minPoll is the fastest the updater asks the control server for commands.
const minPoll = 15 * time.Second
```

Add to `checkSettings`, so the gate refuses bad link settings at start instead of at first use:

```go
	if controlURL != "" {
		if !inside(controlToken, "/etc/bosun") {
			return fmt.Errorf("BOSUN_CONTROL_TOKEN_FILE (%s) must be under /etc/bosun, the only folder the updater can read; mount it there", controlToken)
		}
		if _, err := pollEvery(); err != nil {
			return err
		}
		// The same check the updater makes, so a bad URL or token stops the gate.
		// The host is a stand-in here: the gate only learns the real one when
		// it starts the updater, and the updater checks it for real.
		if _, err := control.New(controlURL, controlToken, "check", controlInsecure); err != nil {
			return err
		}
	}
```

```go
// pollEvery reads BOSUN_CONTROL_POLL.
func pollEvery() (time.Duration, error) {
	d, err := time.ParseDuration(controlPoll)
	if err != nil {
		return 0, fmt.Errorf("BOSUN_CONTROL_POLL %q: %w. Use a value like 60s", controlPoll, err)
	}
	if d < minPoll {
		return 0, fmt.Errorf("BOSUN_CONTROL_POLL %q is under the lowest value %s; use %s or more", controlPoll, minPoll, minPoll)
	}
	return d, nil
}
```

In `runUpdater`, after the notify URLs are loaded:

```go
	ctrl, err := control.New(controlURL, controlToken, os.Getenv("BOSUN_HOST"), controlInsecure)
	if err != nil {
		return err
	}
	u := &updater.Updater{Gate: gate.Dial(filepath.Join(runDir, "gate.sock")), Reg: reg, Notify: updater.Sender(urls), Control: ctrl}
	u.StartEvents(ctx)
	if ctrl != nil && controlCommands {
		poll, err := pollEvery()
		if err != nil {
			return err
		}
		go func() {
			if err := u.Commands(ctx, poll); err != nil {
				log.Printf("commands: %v", err)
			}
		}()
	}
```

and change the ready line:

```go
	link := "off"
	if ctrl != nil {
		link = "events only"
		if controlCommands {
			link = "events and commands"
		}
	}
	log.Printf("updater ready: %d notify URLs, schedule %q, control link %s", len(urls), schedule, link)
```

Add `"time"` and `"github.com/mehrad-meraji/bosun/internal/control"` to `main.go`'s imports.

- [ ] **Step 6: Write the settings tests**

Add to `cli_test.go`:

```go
func TestCheckSettingsControlLink(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "control-token")
	if err := os.WriteFile(tok, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	save := func() func() {
		u, to, p, i := controlURL, controlToken, controlPoll, controlInsecure
		return func() { controlURL, controlToken, controlPoll, controlInsecure = u, to, p, i }
	}
	defer save()()

	controlURL, controlToken, controlPoll, controlInsecure = "https://server", "/srv/token", "60s", false
	if err := checkSettings(); err == nil || !strings.Contains(err.Error(), "/etc/bosun") {
		t.Errorf("a token outside /etc/bosun must be refused: %v", err)
	}

	// With the link off, none of it is checked, whatever the other values say.
	controlURL, controlToken = "", tok
	if err := checkSettings(); err != nil {
		t.Errorf("with the link off, nothing is checked: %v", err)
	}
}

func TestPollEvery(t *testing.T) {
	save := controlPoll
	defer func() { controlPoll = save }()
	for _, tc := range []struct {
		in      string
		wantErr bool
	}{{"60s", false}, {"15s", false}, {"14s", true}, {"", true}, {"soon", true}} {
		controlPoll = tc.in
		if _, err := pollEvery(); (err != nil) != tc.wantErr {
			t.Errorf("pollEvery(%q) = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
	}
}
```

The token-file rule makes the first case easy to test and the happy path hard (it needs a real `/etc/bosun`), so keep the happy path in `TestPollEvery` and the `control.New` checks in `internal/control`'s own tests, which already cover the URL and token rules.

- [ ] **Step 7: Run everything**

Run: `go build ./... && go vet ./... && go vet -tags integration ./... && go test ./... -count=1`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/updater main.go cli_test.go
git commit -m "Control link: poll for commands and run them"
```

---

### Task 9: settings, docs and the spec

**Files:**
- Modify: `README.md`
- Modify: `compose.yml`
- Modify: `docs/superpowers/specs/2026-09-11-bosun-design.md`

**Interfaces:**
- Consumes: everything above.
- Produces: no code.

- [ ] **Step 1: README**

Add a "Control server link" section after the notify section, and add the five settings to the settings table:

```markdown
## Control server link (optional)

Bosun can report to a control server, for example Sentinel, and take two
commands from it. It is off until you set `BOSUN_CONTROL_URL`.

The gate still has no network and opens no port. The updater sends the
events and asks for the commands. The server never connects to Bosun.

```yaml
    environment:
      BOSUN_CONTROL_URL: https://sentinel.example.com/api/bosun
      BOSUN_CONTROL_COMMANDS: "true"
    volumes:
      - ./control-token:/etc/bosun/control-token:ro
```

Events are JSON, one per thing that happened: `update.done`,
`update.rolled_back`, `update.failed`, `version.available`, `gate.refused`,
`registry.failing`, `recovery`, `warning` and `command.result`. They hold no
registry logins, no notify URLs and no env vars.

There are two commands, and no others: `check` runs a round now, and
`skip_clear` lets a container try a skipped version again. A hacked server
cannot pick an image, roll back, or touch your data.

Events wait in memory only. If the updater restarts, events that did not go
out are lost; the next round still works.
```

Settings table rows:

| Setting | Default | Meaning |
|---|---|---|
| `BOSUN_CONTROL_URL` | none | Base URL of the control server. Off when not set. `https://` only, unless `BOSUN_CONTROL_INSECURE=true`. |
| `BOSUN_CONTROL_TOKEN_FILE` | `/etc/bosun/control-token` | File with the bearer token. Must be under `/etc/bosun`. |
| `BOSUN_CONTROL_COMMANDS` | `false` | Ask the server for commands. Events work without it. |
| `BOSUN_CONTROL_POLL` | `60s` | How often to ask. Lowest value `15s`. |
| `BOSUN_HOST` | Docker host name | The host name in events and command polls. |

- [ ] **Step 2: compose.yml**

Add a commented example of the token mount and the two settings, next to the notify file example, so nothing changes for people who do not use the link.

- [ ] **Step 3: Spec fixes**

In `docs/superpowers/specs/2026-09-11-bosun-design.md`, "Control server link":

- The event example: drop `"backup": null` and `"command_id": null`, and add a line under the example: "`backup` is `{ "bytes": 1234, "ms": 900 }` when the update took a backup, and absent when it did not. `command_id` is absent unless a command caused the event. `status` is on `command.result` only: `done`, `busy`, `refused` or `failed`."
- In the sending rules, replace the first bullet with: "The updater tries up to four times: at once, then after 5 s, 30 s and 2 min. A bad token or a bad request is not tried again. Then it drops the event and logs it. Events wait in memory only, so a restart loses events that are not sent. The next round still works."
- Add a bullet: "`steps` come with a finished update only. `update.failed` has no steps; its `reason` says what failed."
- In the commands section, add: "The updater refuses a command list longer than 100, a body over 64 KB, an unknown field, an id that is not 1-128 plain characters, a `check` with a container, and a `skip_clear` whose container is not a valid container name. The gate checks the name again and logs `REFUSED`."

- [ ] **Step 4: Final run**

Run:
```bash
go build ./... && go vet ./... && go vet -tags integration ./... && go test ./... -count=1 -race
go test -tags integration -count=1 ./internal/gate/
```
Expected: all PASS. The integration tests must still pass: Task 1's `updaterBody` change and Task 3's steps touch code they use.

- [ ] **Step 5: Commit**

```bash
git add README.md compose.yml docs/superpowers/specs
git commit -m "Docs: the control server link"
```

---

## What this plan does not build

- **No real-Docker integration test for the link.** The updater has no Docker socket, and the link adds no Docker calls; an `httptest` server plus the fake gate covers the whole flow. Skipped on purpose.
- **No retry queue on disk.** The spec says events live in memory only.
- **No remote `update`, `rollback` or restore.** On purpose, for good.
- **No event for every registry failure.** One event when a registry has failed three rounds in a row, the same rule as the notes.
- **No `warning` event of its own.** The big-backup warning rides in the `update.done` message. The `warning` type is used only for a gate event whose kind Bosun does not know.
