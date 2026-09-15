# Bosun

Bosun keeps your Docker containers up to date. It replaces Watchtower.

**The main difference:** the part that talks to the internet never touches the Docker
socket. The part that touches the socket has no network, and it can only swap a
container's image. Even if the network part is hacked, it cannot take over your host.

- Opt-in: only containers with `bosun.enable=true` are touched.
- Rolls back by itself when a new version is not healthy.
- Manual rollback with one command.
- Notes to Slack, Teams, Discord, ntfy, email and more (Shoutrrr URLs).
- Runs on Linux with Docker Engine, and on a Mac with Docker Desktop.

## Install

1. Find the group that owns the Docker socket, as containers see it:

   ```bash
   export DOCKER_GID=$(docker run --rm -v /var/run/docker.sock:/s busybox stat -c %g /s)
   ```

2. Get `compose.yml` and start Bosun. It pulls the image from `ghcr.io/mehrad-meraji/bosun`:

   ```bash
   curl -fsSLO https://raw.githubusercontent.com/mehrad-meraji/bosun/main/compose.yml
   docker compose up -d
   ```

   To build from source, clone the repo and run `docker compose up -d --build`.

   You will see two containers. `bosun-gate` is yours. `bosun-updater` is made by the
   gate, and the gate removes it when it stops.

   Stopping Bosun waits for a running update to finish (up to 3 minutes).

Bosun keeps its state (skip list, rollback records) in the `bosun-state` volume, which
only the gate can reach.

To upgrade, change the version in `image:` in `compose.yml`, then run `docker compose up -d`.
Bosun does not update itself.

Do not set `hostname` or `user` on the gate. It finds its own container by hostname.

## Where it runs

| Host | Works | Tested by |
|---|---|---|
| Linux, Docker Engine | yes | `scripts/deploy-test.sh` in CI, on every push |
| Mac, Docker Desktop | yes | `scripts/deploy-test.sh`, run by hand |
| Podman | not supported yet | Podman has its own `podman auto-update` |
| Rootless Docker | not supported | the gate needs the normal Docker socket |
| Apple's `container` tool | cannot work | it has no Docker API, which Bosun talks to |

To check your own host, run `scripts/deploy-test.sh`. It starts Bosun, updates a test
app, takes a backup, restores it, rolls back a broken version, and removes everything
it made. It needs port 5066 free.

## Watch a container

| Label | Default | Meaning |
|---|---|---|
| `bosun.enable=true` | off | Watch this container. |
| `bosun.mode=notify` | `update` | Tell only. Do not update. |
| `bosun.health-timeout=120s` | `60s` | How long a new version has to become healthy. |
| `bosun.backup=true` | off | Copy the container's writable volumes before each update. |
| `bosun.logs=true` | off | Send this container's last output to the control server when an update fails. |

Bosun follows the tag. `postgres:16` stays on 16. Images pinned by digest are never
touched.

## Settings

| Setting | Default | Meaning |
|---|---|---|
| `BOSUN_SCHEDULE` | `0 4 * * *` | Cron string for update rounds. |
| `BOSUN_NOTIFY_FILE` | `/etc/bosun/notify.txt` | Shoutrrr URLs, one per line. |
| `BOSUN_INSECURE_REGISTRIES` | none | Comma list of registries allowed over plain HTTP. |
| `BOSUN_CA_FILE` | none | CA certificate for registries with private certificates. |
| `BOSUN_BACKUP_DIR` | `/var/lib/bosun-backups` | Backup folder inside the gate (the `bosun-backups` volume). |
| `BOSUN_BACKUP_WARN_SIZE` | `10GB` | A backup bigger than this adds a one-time warning to the update note. |
| `BOSUN_CONTROL_URL` | none | Base URL of the control server. Off when not set. `https://` only, unless `BOSUN_CONTROL_INSECURE=true`. |
| `BOSUN_CONTROL_TOKEN_FILE` | `/etc/bosun/control-token` | File with the bearer token. Must be under `/etc/bosun`. |
| `BOSUN_CONTROL_COMMANDS` | `false` | Ask the server for commands. The value must be exactly `true`. Events work without it. |
| `BOSUN_CONTROL_POLL` | `60s` | How often to ask. Lowest value `15s`. |
| `BOSUN_CONTROL_INSECURE` | `false` | Allow a plain `http://` control server on your own LAN. The value must be exactly `true`. |
| `BOSUN_HOST` | Docker host name | The host name in events and command polls. |

Registry logins: mount a Docker `config.json` at `/etc/bosun/docker/config.json`. Use a
read-only token made just for Bosun (for GHCR: only `read:packages`). Do not mount your
own `~/.docker/config.json`.

## Backups

With `bosun.backup=true`, Bosun stops the app, copies each writable volume and host
folder with Docker's own copy, then updates. The app stays stopped during the copy,
so big volumes mean long downtime. Bosun keeps one backup per container, in the
`bosun-backups` volume, which only the gate can reach.

To put the data back together with the old version:

```bash
docker exec -it bosun-gate bosun rollback postgres --with-data
```

Data written since the backup is lost. The restore runs a short helper container
from Bosun's own image. It runs as root, because it must delete files and keep file
owners, but it has no network, a read-only root, only three powers (`CHOWN`,
`DAC_OVERRIDE`, `FOWNER`). It only sees that app's folders and the backup folder
(read-only). Don't close the terminal during a restore.

To keep backups in a host folder instead, mount it at `/var/lib/bosun-backups` and
give it to Bosun's user first: `sudo chown 65532:65532 /srv/bosun-backups`.

## Commands

```bash
docker exec -it bosun-gate bosun status
docker exec -it bosun-gate bosun check --dry-run
docker exec -it bosun-gate bosun rollback ls
docker exec -it bosun-gate bosun rollback nginx
docker exec -it bosun-gate bosun skip clear nginx
docker exec -it bosun-gate bosun help rollback
```

## Control server link (optional)

Bosun can report to a control server that you run, and take two
commands from it. It is off until you set `BOSUN_CONTROL_URL`.

The gate still has no network and opens no port. The updater sends the
events and asks for the commands. The server never connects to Bosun.

```yaml
    environment:
      BOSUN_CONTROL_URL: https://control.example.com/api/bosun
      BOSUN_CONTROL_COMMANDS: "true"
    volumes:
      - ./control-token:/etc/bosun/control-token:ro
```

The token file holds the server's bearer token and must not be empty: an
empty file stops Bosun at start-up with a message saying so.

Events are JSON, one per thing that happened: `update.done`,
`update.rolled_back`, `update.failed`, `version.available`, `gate.refused`,
`registry.failing`, `recovery`, `warning` and `command.result`. They hold no
registry logins, no notify URLs and no env vars.

There are two commands, and no others: `check` runs a round now, and
`skip_clear` lets a container try a skipped version again. A hacked server
cannot pick an image, roll back, or touch your data.

### Why an update failed

When an update rolls back, Bosun deletes the new container seconds later, and
its output goes with it. Put `bosun.logs=true` on a container and Bosun reads
the last 50 lines of that container first — and if those are longer than 4 KB,
their last 4 KB, because the end is where a crash says why — then sends them with the
`update.rolled_back` event. The same happens when Bosun restarts after a crash
and throws away a new version that is not healthy: the lines wait in Bosun's
state file until the updater collects them.

Nothing is read at all unless `BOSUN_CONTROL_URL` is set: with no server to
send to, there is nobody to send to.

This is off for every container until you turn it on, one container at a time,
because **app output can hold secrets**. Nothing is hidden or removed from the
lines. Turn it on only for apps whose output you are happy to send to your
control server.

The lines never go anywhere else: not into a Slack or Teams note, not into the
`bosun` commands, not into Bosun's own log. Bosun does not collect logs from a
running container; that is your log tool's job, and Docker can send container
output straight to it with a log driver.

Events wait in memory only. If the updater restarts, events that did not go
out are lost; the next round still works.

## Limits

- A rollback swaps the image only, unless you use `--with-data` with a backup.
- Bosun recreates containers made by compose outside of compose. A later
  `docker compose up` may recreate them again. That is harmless.
- Bosun does not update itself yet. It needs Docker 25 or newer.
- If the Docker daemon restarts in the middle of an update, a container with
  `restart: always` can start twice (old and new) until Bosun recovers. Prefer
  `restart: unless-stopped`.

## License

Apache-2.0. See [LICENSE](LICENSE).
