# Bosun

Bosun keeps your Docker containers up to date. It replaces Watchtower.

**The main difference:** the part that talks to the internet never touches the Docker
socket. The part that touches the socket has no network, and it can only swap a
container's image. Even if the network part is hacked, it cannot take over your host.

- Opt-in: only containers with `bosun.enable=true` are touched.
- Rolls back by itself when a new version is not healthy.
- Manual rollback with one command.
- Notes to Slack, Teams, Discord, ntfy, email and more (Shoutrrr URLs).

## Install

1. Find the group that owns the Docker socket, as containers see it:

   ```bash
   export DOCKER_GID=$(docker run --rm -v /var/run/docker.sock:/s busybox stat -c %g /s)
   ```

2. Start Bosun:

   ```bash
   docker compose up -d --build
   ```

   You will see two containers. `bosun-gate` is yours. `bosun-updater` is made by the
   gate, and the gate removes it when it stops.

Bosun keeps its state (skip list, rollback records) in the `bosun-state` volume, which
only the gate can reach.

Do not set `hostname` or `user` on the gate. It finds its own container by hostname.

## Watch a container

| Label | Default | Meaning |
|---|---|---|
| `bosun.enable=true` | off | Watch this container. |
| `bosun.mode=notify` | `update` | Tell only. Do not update. |
| `bosun.health-timeout=120s` | `60s` | How long a new version has to become healthy. |

Bosun follows the tag. `postgres:16` stays on 16. Images pinned by digest are never
touched.

## Settings

| Setting | Default | Meaning |
|---|---|---|
| `BOSUN_SCHEDULE` | `0 4 * * *` | Cron string for update rounds. |
| `BOSUN_NOTIFY_FILE` | `/etc/bosun/notify.txt` | Shoutrrr URLs, one per line. |
| `BOSUN_INSECURE_REGISTRIES` | none | Comma list of registries allowed over plain HTTP. |
| `BOSUN_CA_FILE` | none | CA certificate for registries with private certificates. |

Registry logins: mount a Docker `config.json` at `/etc/bosun/docker/config.json`. Use a
read-only token made just for Bosun (for GHCR: only `read:packages`). Do not mount your
own `~/.docker/config.json`.

## Commands

```bash
docker exec -it bosun-gate bosun status
docker exec -it bosun-gate bosun check --dry-run
docker exec -it bosun-gate bosun rollback ls
docker exec -it bosun-gate bosun rollback nginx
docker exec -it bosun-gate bosun skip clear nginx
docker exec -it bosun-gate bosun help rollback
```

## Limits

- A rollback swaps the image only. It does not undo data changes.
- Bosun recreates containers made by compose outside of compose. A later
  `docker compose up` may recreate them again. That is harmless.
- Bosun does not update itself yet. It needs Docker 25 or newer.
- If the Docker daemon restarts in the middle of an update, a container with
  `restart: always` can start twice (old and new) until Bosun recovers. Prefer
  `restart: unless-stopped`.
