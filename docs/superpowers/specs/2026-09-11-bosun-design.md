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
2. **Backup helper**: a short-lived container from Bosun's own image. See "Backups".
3. **Updater**: Bosun's own updater, with fixed settings. See "Deploy".

The gate refuses anything else, logs it loudly, and sends a note. A refusal means a bug
or an attack. It is never silent.

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

The gate has no network, so it cannot send notes. Results come back to the updater in
the `update` reply. Events that happen when no call is open (crash recovery on gate
start) wait in a small queue until the updater calls `events()`.

The CLI commands (`status`, `rollback`, `skip`, `check`) run inside the gate container
with `docker exec`, so they call Docker directly. The gate daemon and the CLI share one
lock file in `/run/bosun`. Only one of them changes containers or the state file at a
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
file in the shared volume (`/run/bosun/state.json`), written only by the gate. It
holds:

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
- One backup per container, matching the one kept old image.
- Stored in `BOSUN_BACKUP_DIR` on the host.
- Before a backup, the gate checks free space. If there is not enough, it skips the
  update and reports.

### Backup helper container

- Runs Bosun's own image (by ID) with the `backup-helper` command. No other tools come
  in.
- Mounts the app's volumes read-only (read-write only for a restore), plus the backup
  folder.
- `network_mode: none`, read-only root file system, `no-new-privileges`.
- **Runs as the same user as the app. It never gets more power than the app already
  has.** Many official images (for example `postgres`) start as root. For those, the
  helper also runs as root, inside the limits above. It gets only the capabilities it
  needs to read, write, and keep file owners.
- Removed when the copy ends.

## Warnings

On gate start, and in `status`, Bosun warns for each watched container that:

- has `bosun.backup=true` and runs as root: "backup helper for postgres will run as
  root (no network, only its own volumes)",
- has `bosun.backup=true` and volumes over `BOSUN_BACKUP_WARN_SIZE` (default `10GB`):
  "backup for jellyfin is 180 GB, expect long downtime".

These warnings go out as a note one time only. There is no confirm step, because the
user already chose these with a label.

## Settings

### Labels on your containers

| Label | Default | Meaning |
|---|---|---|
| `bosun.enable=true` | off | Watch this container. |
| `bosun.mode=notify` | `update` | Tell only. Do not update. |
| `bosun.backup=true` | off | Back up volumes before each update. |
| `bosun.health-timeout=120s` | `60s` | How long to wait for healthy. |

### Settings on the gate

| Setting | Default | Meaning |
|---|---|---|
| `BOSUN_SCHEDULE` | `0 4 * * *` | Cron string for rounds. |
| `BOSUN_NOTIFY_FILE` | none | File with Shoutrrr URLs, one per line. A file, not an env var, because the URLs hold tokens and env vars show in `docker inspect`. Works with Docker secrets. |
| `BOSUN_BACKUP_DIR` | none | Host folder for backups. Needed if any container uses `bosun.backup`. |
| `BOSUN_BACKUP_WARN_SIZE` | `10GB` | Size that triggers the long-downtime warning. |
| `BOSUN_REGISTRY_AUTH` | none | Path to a Docker `config.json`. |
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

## CLI

Run inside the gate:

```bash
docker exec -it bosun-gate bosun <command>
```

| Command | Does |
|---|---|
| `status` | Watched containers, mode, backup on or off, root helper or not, backup size, last downtime, warnings. |
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

## Testing

- **Gate rules** (the security core): table tests in plain Go. Covers the RPC input
  checks and the recreate builder. For example: the recreate output must never add
  `privileged`, a mount, a capability, a device, `network_mode: host`, `pid: host`, or
  a user change, whatever the input.
- **Recreate rules**: tests that image defaults are not copied, that user overrides
  are kept, and that anonymous volumes and all networks carry over.
- **Fuzz tests** with Go's built-in fuzzing on the RPC message reader.
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

## Not in v1

- Updating Bosun itself. It only reports a new version.
- Start order for containers that depend on each other.
- Version jumps (16 to 17, semver rules).
- Registry credential helpers, so no AWS ECR or Google GCR short-lived logins.
- Keeping more than one old version.
- A rollback button in notes (needs an open port).
- Rootless Docker and Podman. Podman users have `podman auto-update`.
