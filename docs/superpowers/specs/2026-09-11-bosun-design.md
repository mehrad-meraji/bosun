# Bosun design

Date: 2026-09-11
Status: approved in brainstorm, waiting for spec review

## What it is

Bosun keeps Docker containers up to date. It replaces Watchtower, which is no longer
maintained and ran with full root power through the Docker socket.

Bosun's main idea: **the part that talks to the internet never touches the Docker
socket, and the part that touches the socket can only swap images.** Even if the
network-facing part is hacked, it cannot take over the host.

Audience: one person's servers and homelab first. Built clean enough to go public
later.

## Security model

Any process that can use `/var/run/docker.sock` has root power on the host. It can
start a container that mounts `/`. Running as a non-root user does not change that.
So Bosun splits the work in two and limits what the socket holder will do.

- **Gate.** The only process that calls Docker. No network. It does not take Docker
  API requests from anyone. It offers a small, typed set of actions, and it builds
  every Docker request itself.
- **Updater.** Talks to registries and notification services. No Docker socket. It
  can only ask the gate for those typed actions.

The gate creates a container in only three cases:

1. **Recreate**: the same container again. Only the image changes (plus Bosun's own
   bookkeeping). The gate builds this request from the old container, so no caller
   can add mounts, `privileged`, capabilities, devices, `network_mode: host`,
   `pid: host`, or a different user.
2. **Restore helper**: a short-lived container from Bosun's own image, only for
   `rollback --with-data`. See "Backups".
3. **Updater**: Bosun's own updater, with fixed settings. See "Deploy".

The gate refuses anything else and logs it loudly as `REFUSED`. The updater also gets
the refusal as an error and sends a note. A refusal means a bug or an attack. The gate
has no network, so a hacked updater could hide the note. The gate log is the record
you can trust.

## Deploy

One image, one Go binary (`bosun`). You add **one service** to compose: the gate.

### Gate container (`bosun-gate`)

- Mounts `/var/run/docker.sock`. Nothing else does.
- Runs as a non-root user. It joins only the host's `docker` group through
  `group_add: ["${DOCKER_GID}"]`. The README gives the one-liner to find it:
  `stat -c %g /var/run/docker.sock`.
- `network_mode: none`.
- Listens on a unix socket in a shared named volume: `/run/bosun/gate.sock`.
- Hardened: read-only root file system, `cap_drop: [ALL]`,
  `security_opt: [no-new-privileges:true]`, distroless image with no shell.

### Updater container (`bosun-updater`), made by the gate

On start, the gate:

1. Finds its own container ID and inspects itself.
2. Removes any old updater (label `bosun.managed-by=bosun-gate`).
3. Creates the updater from its own image **ID**, not its tag. This stops a moved tag
   from swapping in a different image.
4. Gives it: the shared `/run/bosun` volume, and read-only copies of the gate's bind
   mounts for the registry `config.json`, the notify file, and the CA file.
5. Never gives it `docker.sock`.
6. Same hardening as the gate, but with a normal network, so it can reach registries
   and notification services.

When the gate stops, it removes the updater.

Known cost: `docker ps` shows a container that is not in your compose file. Its name
and label make its source clear.

## Gate RPC

A small request/response protocol over `/run/bosun/gate.sock`. Messages are small JSON
objects with fixed fields. Unknown fields are refused.

| Call | Does |
|---|---|
| `list()` | Returns watched containers: name, image ref, local digest, labels, skip list. |
| `update(name, digest, registryAuth)` | Runs the full update sequence for one container. Returns the result. |
| `events()` | Returns and clears queued events, such as the result of a crash recovery. |
| `skipClear(name)` | Removes a container's versions from the skip list. Only for the control server link (see "Control server link"). |

With the control server link, the `update` reply also gives a `steps` list.

The gate has no network, so it cannot send notes. Results come back to the updater in
the `update` reply. Events that happen when no call is open (crash recovery on gate
start) wait in a small queue until the updater calls `events()`.

The CLI commands (`status`, `rollback`, `skip`, `check`) run inside the gate container
with `docker exec`, so they call Docker directly. The gate daemon and the CLI share one
lock file in `/var/lib/bosun`. Only one of them changes containers or the state file at a
time. A `rollback` during a round is refused, like a second `check`.

Gate and updater run as the same non-root user ID, so only they can open `gate.sock`
(file mode `0600`).

## Update round

**When:** on a cron schedule. The default is `0 4 * * *`. Only one round runs at a
time.

### Check (updater)

1. Call `list()` for containers with `bosun.enable=true`.
2. For each one, send a `HEAD` request to the registry for its tag's manifest digest.
   `HEAD` does not count against Docker Hub rate limits.
3. Do nothing if the digest matches the local one, or is on the skip list.
4. Follow the tag only. `postgres:16` stays on 16. No jumps to 17.
5. Never touch images pinned by digest (`image@sha256:...`).
6. For `bosun.mode=notify`: send a "new version ready" note and stop.
7. Otherwise call `update(name, digest, auth)`. One container at a time.

### Update (gate)

1. **Pull** `repo:tag` with the given auth. Check the pulled digest equals the given
   digest. If not (the tag moved between check and pull), stop and report. The old
   container is still running, so there is no downtime yet.
2. **Stop** the old container.
3. **Back up** its volumes, if `bosun.backup=true`. If the backup fails, start the old
   container again, report, and stop.
4. **Rename** the old container with a unique suffix, like `nginx-bosun-1a2b3c`.
   Record the pair in the state file.
5. **Create** the new container with the old name and the new image. Start it.
6. **Wait for healthy.** Use the image's `HEALTHCHECK` if it has one. If not,
   "healthy" means still running with no restarts for the health timeout. The default
   timeout is 60s.
7. **If healthy:** remove the renamed old container. Tag the old image as
   `bosun/prev/<name>:<short-digest>` so `docker image prune` does not remove it.
   Remove the tag of any older kept version. Report success with downtime.
8. **If not healthy:** stop and remove the new container. Rename the old one back and
   start it. Add the new digest to the skip list. Report the rollback.

### Recreate rules

`docker inspect` shows the container's settings merged with defaults from its image.
Copying those as-is would freeze old image defaults forever. For example, a new image
with a new `CMD` would be ignored. So the gate:

- Compares the container's `Config` with the old image's `Config`, and keeps only the
  values the user set: `Env`, `Cmd`, `Entrypoint`, `WorkingDir`, `User`,
  `ExposedPorts`, `Volumes`, `Healthcheck`, `Labels`. The new image gives the rest.
- Copies `HostConfig` as-is.
- Reattaches every entry in `Mounts`, including anonymous volumes by their generated
  name, so no data is left behind.
- Copies every network in `NetworkSettings.Networks`, with aliases and static IPs.

## State

Docker labels cannot change after a container is made. So Bosun keeps a small state
file, `/var/lib/bosun/state.json`, in its own `bosun-state` volume. Only the gate mounts
that volume. The updater never gets it, so a hacked updater cannot forge crash records.
The shared `/run/bosun` volume holds only the two sockets. It holds:

- the kept old image per container,
- the skip list,
- in-progress renames (for crash recovery),
- last downtime per container,
- which one-time warnings were already sent.

Labels hold only the user's settings. If the state file is lost, Bosun loses the skip
list and the rollback records. Containers keep running, and the next round works.

## Backups

Opt-in per container with `bosun.backup=true`.

- Run while the app is stopped (step 3 above), so database files are in a clean state.
- What is backed up: every writable mount of the container, both volumes (named and
  anonymous) and host folders. Read-only mounts and `*.sock` files are skipped.
- **How:** the gate asks Docker for a tar copy of each mount from the stopped
  container (`GET /containers/{id}/archive`). Docker keeps file owners and modes. No
  helper container runs for a backup.
- One backup per container, matching the one kept old image. A `manifest.json` records
  the image ID, the time, and each mount's path and size.
- Stored in `BOSUN_BACKUP_DIR` inside the gate (default `/var/lib/bosun-backups`, the
  `bosun-backups` volume). Only the gate mounts it. The updater never gets it.
- The new backup is written next to the old one and only replaces it when complete.
- Free space: before a backup, the gate checks there is at least as much free space as
  the last backup used. If the disk fills during the copy, the gate stops, deletes the
  partial copy, starts the old container again, and reports.

### Restore helper container

Docker's copy can add and overwrite files, but it cannot delete them. A restore must
also remove files written after the backup, so it uses a small helper:

- Runs only for `rollback <name> --with-data`, which a person types and confirms.
- Runs Bosun's own image (by ID) with the `restore-helper` command. No other tools come
  in.
- Mounts the app's backed-up mounts read-write, and the backup folder read-only.
- Runs as root, because it must delete any file and keep file owners. It is locked
  down: `network_mode: none`, read-only root file system, `no-new-privileges`, all
  capabilities dropped except `CHOWN`, `DAC_OVERRIDE`, and `FOWNER`.
- Checks every tar file is readable before it deletes anything.
- Removed when the restore ends.
- Order: stop the app, restore the data, then swap to the old image. If the old
  version does not come up healthy, the gate leaves the container stopped (never the
  new version on old data) and says so.

## Warnings

- A backup larger than `BOSUN_BACKUP_WARN_SIZE` (default `10GB`) adds a warning to that
  update's note one time only: "backup for jellyfin is 180 GB, expect long downtime".
- `status` shows the last backup size for each container with backups on.
- `rollback show <name> --with-data` says the restore helper runs as root, with no
  network, and only sees that app's folders.

There is no confirm step for updates, because the user already chose backups with a
label.

## Settings

### Labels on your containers

| Label | Default | Meaning |
|---|---|---|
| `bosun.enable=true` | off | Watch this container. |
| `bosun.mode=notify` | `update` | Tell only. Do not update. |
| `bosun.backup=true` | off | Back up volumes before each update. |
| `bosun.health-timeout=120s` | `60s` | How long to wait for healthy. |
| `bosun.logs=true` | off | Send this container's last output to the control server when an update fails. |

### Settings on the gate

| Setting | Default | Meaning |
|---|---|---|
| `BOSUN_SCHEDULE` | `0 4 * * *` | Cron string for rounds. |
| `BOSUN_NOTIFY_FILE` | `/etc/bosun/notify.txt` | File with Shoutrrr URLs, one per line. A file, not an env var, because the URLs hold tokens and env vars show in `docker inspect`. Works with Docker secrets. |
| `BOSUN_BACKUP_DIR` | `/var/lib/bosun-backups` | Backup folder inside the gate (the `bosun-backups` volume). To use a host folder, mount it here and `chown 65532` it first. Must not be under `/run/bosun` or `/etc/bosun`. |
| `BOSUN_BACKUP_WARN_SIZE` | `10GB` | Size that triggers the long-downtime warning. |
| Registry logins | none | Mount a Docker `config.json` at `/etc/bosun/docker/config.json`. Everything under `/etc/bosun` is passed to the updater read-only. |
| `BOSUN_INSECURE_REGISTRIES` | none | Comma list of registries allowed over plain HTTP. |
| `BOSUN_CA_FILE` | none | CA certificate for registries with private certificates. |

## Private registries

- Any registry with the Docker Registry v2 API: Docker Hub, GHCR, GitLab, Harbor,
  Gitea, Quay, self-hosted `registry:2`.
- Registry calls use `github.com/google/go-containerregistry`. It handles login tokens
  and `HEAD` digest checks.
- Logins come from a Docker `config.json` with an `auths` section, mounted read-only
  into the updater only.
- The updater passes the login to the gate with each `update` call. The gate passes it
  to Docker for the pull, then forgets it. It never stores or logs it.
- HTTPS only, unless the registry is in `BOSUN_INSECURE_REGISTRIES`.
- The docs recommend a separate read-only token (for GHCR: only `read:packages`), not
  your full `~/.docker/config.json`.

## Notifications

Sent by the updater only, with the Shoutrrr fork `github.com/nicholas-fedor/shoutrrr`
(active; v0.20.0 released 2026-09-08). Pin the version. It covers Slack, Teams,
Discord, Telegram, ntfy, Gotify, email, and generic webhooks, with the same URL format
Watchtower used.

Notes go out for:

- update done (with downtime, and backup time if any),
- update failed and rolled back,
- new version ready (`notify` mode),
- gate refused a request,
- a registry failed 3 rounds in a row for a container,
- crash recovery results,
- one-time warnings.

A failed note is logged only. It never blocks an update.

## Control server link

Status: added 2026-09-11, after the core plan. Build it after the core works.

Bosun can link to a control server, for example Sentinel. The link has two parts:
**events** (Bosun tells the server what happened, as JSON) and **commands** (the
server asks Bosun to do a small set of things). Both are off until you set
`BOSUN_CONTROL_URL`.

The main rule stays the same: **the gate has no network and opens no port.** The
updater makes every call. It sends events out, and it asks the server for commands.
The server never connects to Bosun.

### Events (JSON)

Notes are plain text for people. A server needs fixed fields. So, for each thing that
makes a note, the updater also sends one JSON event:

```
POST {BOSUN_CONTROL_URL}/events
Authorization: Bearer <token from BOSUN_CONTROL_TOKEN_FILE>
Content-Type: application/json
```

```json
{
  "schema": 1,
  "id": "0f8c2c1e-5b7a-4d0e-9a51-3c2d7e6f1a90",
  "time": "2026-09-11T04:01:33Z",
  "host": "worker-1",
  "bosun_version": "0.2.0",
  "type": "update.rolled_back",
  "container": "redis",
  "image": "redis:7.4",
  "from_digest": "sha256:b20c...",
  "to_digest": "sha256:9ae1...",
  "downtime_ms": 38000,
  "steps": [
    { "name": "pull", "status": "ok", "ms": 6200 },
    { "name": "stop", "status": "ok", "ms": 1100 },
    { "name": "backup", "status": "skipped", "detail": "bosun.backup is off" },
    { "name": "start", "status": "ok", "ms": 800 },
    { "name": "health", "status": "failed", "ms": 60000, "detail": "restarted 3 times, exit code 1" },
    { "name": "rollback", "status": "ok", "ms": 1300 },
    { "name": "skip", "status": "ok", "detail": "sha256:9ae1... added to the skip list" }
  ],
  "reason": "health check failed"
}
```

`backup` is `{ "bytes": 1234, "ms": 900 }` when the update took a backup, and
absent when it did not. `command_id` is absent unless a command caused the
event. `status` is on `command.result` only: `done`, `busy`, `refused` or
`failed`.

Event types: `update.done`, `update.rolled_back`, `update.failed` (nothing was
stopped), `version.available` (notify mode), `gate.refused`, `registry.failing`,
`recovery`, `warning`, `command.result`.

- `id` is new for each event. The server uses it to drop repeats.
- `host` comes from the gate. The gate reads Docker's host name once and puts it in
  the updater's env. `BOSUN_HOST` on the gate overrides it.
- `steps` come from the gate. The `update` reply gets a `steps` list with name, status,
  time and a short detail for each step of the update sequence.
- `update.failed` has no `steps`: when the gate returns an error instead of a result,
  there is no honest step list, and the `reason` says what failed.
- `command_id` is set when a command caused the event.
- `logs` holds the last 50 lines (up to 4 KB) of a container Bosun threw away, and only
  for a container with `bosun.logs=true`. It is set on `update.rolled_back`, and on a
  `recovery` event when crash recovery removed an unhealthy new version. Bosun reads it
  before the container is deleted, because nothing can read it afterwards. Nothing is
  redacted: the label is the consent. The lines go to the control server only — never to
  a note, the CLI or the gate log. Bosun never collects the output of a running
  container; a log tool or a Docker log driver does that.
- The body never holds registry logins, notify URLs or env vars.

Sending rules:

- The updater tries up to four times: at once, then after 5 s, 30 s and 2 min. A bad
  token or a bad request is not tried again. Then it drops the event and logs it.
  Events wait in memory only, so a restart loses events that are not sent. The next
  round still works.
- A failed event never blocks an update. Same rule as notes.
- Notes still go out as before. Events do not replace them.

### Commands (pull, no open port)

When `BOSUN_CONTROL_COMMANDS=true`, the updater asks the server for commands every
`BOSUN_CONTROL_POLL` (default `60s`):

```
GET {BOSUN_CONTROL_URL}/commands?host=worker-1
Authorization: Bearer <token>
```

The reply is a JSON list. Each command has fixed fields. Unknown fields or unknown
commands are refused and logged, and they get a `command.result` event with
`status: refused`.

The updater refuses a command list longer than 100, a body over 64 KB, an unknown
field, an id that is not 1-128 plain characters, a `check` with a container, and a
`skip_clear` whose container is not a valid container name. The gate checks the name
again and logs `REFUSED`.

| Command | Fields | Does |
|---|---|---|
| `check` | `id` | Runs a round now. If a round is running, the result is `busy`. |
| `skip_clear` | `id`, `container` | Removes that container's versions from the skip list. It does not start an update. Send `check` after it to try again. |

That is the full list. On purpose, there is **no remote `update`, `rollback` or
restore**:

- `check` only does what the next cron round would do.
- `skip_clear` only lets a round try a version again. If that version is still bad, the
  health check rolls it back again.
- So a hacked server can make Bosun do early rounds and retry a known-bad version. It
  cannot pick an image, update a `notify` container, roll back, or touch data.

How a command runs:

1. The updater reads the reply. It keeps only commands with an `id` it has not seen in
   the last 24 hours. This stops a replay from running a command twice.
2. `check` runs in the updater, like a cron round.
3. `skip_clear` goes to the gate as a new typed RPC call, `skipClear(name)`. The state
   file belongs to the gate, so only the gate changes it. The gate uses the same lock as
   the CLI.
4. The updater sends a `command.result` event with the `command_id` and one of `done`,
   `busy`, `refused` or `failed`. The server uses it to remove the command.

A command runs up to one poll interval late. That is the cost of having no open port.

### Link settings (on the gate, passed to the updater)

| Setting | Default | Meaning |
|---|---|---|
| `BOSUN_CONTROL_URL` | none | Base URL of the control server. Off when not set. HTTPS only, unless `BOSUN_CONTROL_INSECURE=true` (for a server on your own LAN). |
| `BOSUN_CONTROL_TOKEN_FILE` | `/etc/bosun/control-token` | File with the bearer token. A file, not an env var, for the same reason as the notify file. |
| `BOSUN_CONTROL_COMMANDS` | `false` | Ask the server for commands. Events work without it. |
| `BOSUN_CONTROL_POLL` | `60s` | How often to ask for commands. Lowest value `15s`. |
| `BOSUN_HOST` | Docker host name | Host name sent in events and command polls. |

## CLI

Run inside the gate:

```bash
docker exec -it bosun-gate bosun <command>
```

| Command | Does |
|---|---|
| `status` | Watched containers, mode, backup on or off, last backup size, last downtime. |
| `check --dry-run` | Run a round now, but only print what it would do. |
| `check` | Run a round now. Refuses if a round is running. |
| `rollback ls` | Containers that can go back: now, back to, when updated, data backup and size. |
| `rollback show <name>` | What a rollback will do, including "data written in the last 3 days will be lost" when data restore is on. |
| `rollback <name>` | Go back one version. Asks `Continue? [y/N]`. |
| `rollback <name> --with-data` | Also restore volumes from the backup. |
| `rollback <name> --dry-run` | Only print the steps. |
| `rollback <name> --yes` | Skip the question. |
| `skip ls` | Versions on the skip list. |
| `skip clear <name>` | Allow a skipped version again. |
| `help <command>` | Short help with one example. |

A rollback adds the current version to the skip list, so the next round does not
update back to it.

Every error says what to do next. For example: "No old version kept for nginx. Run
`bosun rollback ls` to see what you can roll back."

## Errors

| Case | What happens |
|---|---|
| Pull fails or digest mismatch | Nothing stopped yet. Skip this container this round. |
| Registry down or rate-limited | Skip this container. Note after 3 rounds in a row. |
| Backup fails or not enough space | No update. Old container started again. Note. |
| Crash or power loss mid-update | On gate start, read in-progress renames from the state file. If the new container runs and is healthy, remove the old one. If not, put the old one back. Queue an event for the updater. |
| Gate refuses a request | Log loudly. Note. |
| Note fails | Log only. |
| Second round while one runs | Refused with "a round is running, try again later". |
| Control server down | Events: four tries, then dropped and logged. Commands: try again at the next poll. Updates go on. |
| Bad command from the control server | Refused, logged, and a `command.result` event with `refused`. |

## Testing

- **Gate rules** (the security core): table tests in plain Go. Covers the RPC input
  checks and the recreate builder. For example: the recreate output must never add
  `privileged`, a mount, a capability, a device, `network_mode: host`, `pid: host`, or
  a user change, whatever the input.
- **Recreate rules**: tests that image defaults are not copied, that user overrides
  are kept, and that anonymous volumes and all networks carry over.
- **Fuzz tests** with Go's built-in fuzzing on the RPC message reader.
- **Control server link**: table tests that the command reader refuses unknown
  commands and unknown fields, and drops a repeated `id`. A golden test for the event
  JSON. A test that nothing is sent when `BOSUN_CONTROL_URL` is not set. A fuzz test on
  the command reader.
- **Full-flow tests** on real Docker in GitHub Actions, with a local `registry:2`:
  - push v1, run it, push v2: it updates;
  - push a broken v3 that exits at once: it rolls back and skips v3;
  - write a file to a volume, update, `rollback --with-data`: the file comes back;
  - kill the gate mid-update: the restart fixes it;
  - new image with a changed `CMD`: the new `CMD` is used.

## Known limits

- Bosun recreates containers made by compose outside of compose. A later
  `docker compose up` may recreate them again. This is harmless.
- A rollback swaps the image only. It does not undo data changes unless
  `--with-data` is used with a backup.
- Updates with backups have longer downtime, because the app stays stopped during the
  copy.
- Do not close the terminal during `rollback --with-data`. If the restore is cut off,
  the app stays stopped with partial data; run the same command again.
- Mounts nested inside another backed-up mount are copied twice.

## Not in v1

- Updating Bosun itself. It only reports a new version.
- Start order for containers that depend on each other.
- Version jumps (16 to 17, semver rules).
- Registry credential helpers, so no AWS ECR or Google GCR short-lived logins.
- Keeping more than one old version.
- A rollback button in notes (needs an open port). The control server link can clear
  the skip list and start a round, but it cannot roll back.
- Rootless Docker and Podman. Podman users have `podman auto-update`.
