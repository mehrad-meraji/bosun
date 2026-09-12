# Handover prompt — the Sentinel side of the Bosun link

Paste everything below the line to the agent working on Sentinel.

---

Build the Sentinel side of the Bosun link.

**Bosun** is a Docker container updater (a safer Watchtower) at `/Users/mehrad/Projects/bosun`.
It is finished and shipped on `main`. It can already talk to a control server; Sentinel is
that server, and its half does not exist yet. Your job is the Sentinel half: the two HTTP
endpoints, the storage, and the UI.

Read Bosun's design at `/Users/mehrad/Projects/bosun/docs/superpowers/specs/2026-09-11-bosun-design.md`,
section "Control server link", and the code in `/Users/mehrad/Projects/bosun/internal/control/`.
That code is the contract. Where this prompt and the code disagree, the code wins — say so
rather than guessing.

## How the link works, and why it is shaped this way

Bosun runs two processes from one binary. The **gate** is the only one that touches
`docker.sock`, and it has **no network at all**. The **updater** has network and no socket.
So:

- **Sentinel never connects to Bosun.** Bosun has no open port. Every call is outbound from
  the updater: it posts events, and it asks for commands.
- **A command runs up to one poll interval late** (60s by default). That is the price of
  having no open port. Do not design anything that needs Bosun to act instantly.
- **There are two commands and there will never be more:** `check` and `skip_clear`. A
  hacked Sentinel must only be able to cause an early update round and a retry of a version
  Bosun already refused. It must not be able to pick an image, update a notify-only
  container, roll back, restore data, or touch a volume. If a Sentinel feature seems to need
  more, that is a no: raise it instead of designing around it.

## Endpoint 1: events

```
POST {BOSUN_CONTROL_URL}/events
Authorization: Bearer <token>
Content-Type: application/json
```

One event per request. Reply with 2xx and an empty body. Details that matter:

- **Never redirect.** Bosun refuses a 3xx and reports the event as failed. (Go would turn
  the POST into a bodiless GET, so the refusal is deliberate.)
- **Never echo the request body or the token** in an error reply. Bosun does not keep reply
  bodies, but do not make it a habit.
- **Retries:** Bosun tries up to four times — at once, then after 5s, 30s and 2 min — for a
  network error, a 5xx or a 429. Any other 4xx is not retried; the event is dropped and
  logged. So answer 429 when you are overloaded, never 400 for something transient.
- **20-second timeout per request.** Answer fast; do the work after you have stored the row.
- **Events are in memory only on Bosun's side.** If the updater restarts, unsent events are
  gone. Sentinel must tolerate gaps, duplicates and out-of-order arrivals.
- **Drop repeats by `id`.** A retried event carries the same `id` as the first attempt.

### The event body

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
  "backup": { "bytes": 1234567, "ms": 900 },
  "steps": [
    { "name": "pull",   "status": "ok",      "ms": 6200 },
    { "name": "stop",   "status": "ok",      "ms": 1100 },
    { "name": "backup", "status": "skipped", "detail": "bosun.backup is off" },
    { "name": "start",  "status": "ok",      "ms": 800 },
    { "name": "health", "status": "failed",  "ms": 60000, "detail": "restarted 3 times, exit code 1" },
    { "name": "rollback", "status": "ok",    "ms": 1300 },
    { "name": "skip",   "status": "ok",      "detail": "sha256:9ae1... added to the skip list" }
  ],
  "reason": "the new version did not come up healthy",
  "status": "done",
  "command_id": "c-1735",
  "logs": "boom: no such table\nexiting\n"
}
```

Field by field:

| Field | Always? | Meaning |
|---|---|---|
| `schema` | yes | Format version, `1` today. Reject an unknown major and say so in the reply. |
| `id` | yes | New per event. **Your dedupe key.** A retry reuses it. |
| `time` | yes | RFC 3339 UTC. When the thing happened, not when it was sent. |
| `host` | yes | The Docker host name, or `BOSUN_HOST`. Your fleet key. |
| `bosun_version` | yes | Bosun's version. |
| `type` | yes | One of the nine below. |
| `container` | usually | Container name. Absent on `command.result`. |
| `image` | usually | Image ref with tag, e.g. `redis:7.4`. |
| `from_digest` / `to_digest` | when known | The version it ran / the version it went to. |
| `downtime_ms` | on a finished update | How long the app was down. |
| `backup` | only with a backup | `bytes` and `ms` of the volume copy. Absent when there was none. |
| `steps` | on a finished update | The update sequence in order. Names: `pull`, `stop`, `backup`, `start`, `health`, `rollback`, `skip`. Statuses: `ok`, `failed`, `skipped`. `ms` is that step's own time and is absent on a skipped step. **`update.failed` has no steps** — the gate returned an error instead of a result, so there is no honest list; `reason` says what failed. |
| `reason` | on anything that went wrong | Free text for a person. Do not parse it. |
| `status` | `command.result` only | `done`, `busy`, `refused` or `failed`. |
| `command_id` | when a command caused it | Matches the `id` you handed out. |
| `logs` | rarely | The last output of a container Bosun threw away. See the warning below. |

### The nine event types

| Type | When |
|---|---|
| `update.done` | An update finished and the new version is healthy. |
| `update.rolled_back` | The new version failed its health check; the old one is back and the new digest is on the skip list. |
| `update.failed` | The update never got going (a pull failed, the tag moved, a backup failed). **Nothing was stopped.** No steps. |
| `version.available` | A notify-only container has a new version. Bosun will not install it. |
| `gate.refused` | The gate refused a request. This means a bug or an attack — surface it loudly. |
| `registry.failing` | A registry has failed three rounds in a row for one container. |
| `recovery` | Bosun restarted after a crash and finished or undid a cut-off update. `reason` says what it did. |
| `warning` | Anything else Bosun wants a person to see. |
| `command.result` | The outcome of one of your commands. Carries `command_id` and `status`. |

### `logs` — treat as secret

`logs` holds up to 4 KB of a container's own output, and it appears only when the user put
`bosun.logs=true` on that container. **Nothing in it is redacted or masked.** An app that
prints a database URL or a token at start-up prints it into this field. In Sentinel:

- store it where your log data's access rules apply, not in an event blob anyone can read;
- never put it in a notification, a digest email, or a page title;
- do not index it for full-text search across projects unless your log feature already does
  that for log data.

Bosun never sends the output of a **running** container. Collecting those is Sentinel's log
feature's job, through Docker log drivers or a log agent — Bosun only rescues the words of a
container it is about to delete, which no log tool can reach in time.

## Endpoint 2: commands

```
GET {BOSUN_CONTROL_URL}/commands?host=worker-1
Authorization: Bearer <token>
```

Reply with a JSON **list**, and only these shapes:

```json
[
  { "id": "c-1735", "type": "check" },
  { "id": "c-1736", "type": "skip_clear", "container": "nginx" }
]
```

Bosun's reader is strict, and a mistake costs you the whole reply or that item:

- **An unknown field anywhere in an item refuses that item** (and Bosun reports it as
  `refused`). Do not add fields. Do not send `null` extras.
- **At most 100 items, and at most 64 KB of body.** Over either and Bosun refuses the whole
  reply and runs nothing.
- **A body that is not a JSON list** is refused whole. `{}` is not a list.
- **`id`** must match `^[a-zA-Z0-9_.:-]{1,128}$`. An item with a bad or missing id is
  dropped silently — you get no `command.result`, so never use, say, a UUID with braces.
- **`check`** takes no `container`. Sending one refuses the item.
- **`skip_clear`** needs `container` matching `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`.
- **An empty list is the normal answer.** Bosun polls every 60s by default (15s floor).

### The command lifecycle, which is where the bugs will be

1. Bosun polls and reads your list.
2. It drops any id it has already **finished** in the last 24 hours.
3. It runs what is left, then posts a `command.result` event per id.
4. **Keep offering a command until you see its `command.result`.** That is your only signal
   it happened.
5. On `status: "busy"` (an update round was already running) Bosun has **not** remembered the
   id: keep the command on the list and it runs on a later poll. Do not treat `busy` as
   finished.
6. On `done`, `failed` or `refused`, take it off the list. Re-sending that id inside 24 hours
   does nothing.
7. **Many `check` commands in one reply run one round**, and every one of their ids gets the
   same result. Do not queue five checks expecting five rounds.
8. A `skip_clear` starts no update. It only lets a later round try that version again. Send a
   `check` after it if the user wants it now.

## Settings the user sets on Bosun's side (for your docs and setup screen)

| Setting | Default | Meaning |
|---|---|---|
| `BOSUN_CONTROL_URL` | none | Your base URL, e.g. `https://sentinel.example.com/api/bosun`. The link is off when unset. `https://` only, unless `BOSUN_CONTROL_INSECURE=true`. No query or `#fragment` allowed. |
| `BOSUN_CONTROL_TOKEN_FILE` | `/etc/bosun/control-token` | File holding the bearer token. Must be under `/etc/bosun` and must not be empty. |
| `BOSUN_CONTROL_COMMANDS` | `false` | Ask for commands. Events work without it. Must be exactly `true`. |
| `BOSUN_CONTROL_POLL` | `60s` | How often to ask. 15s floor. |
| `BOSUN_HOST` | Docker host name | The `host` value in events and polls. |

So Sentinel's setup screen should hand the user: the base URL, a token to paste into a file,
and the compose lines for those settings.

## What to build

Decide the shape with the user, but the pieces are:

1. **`POST /events`** — authenticate the bearer token per host, dedupe by `id`, store, answer
   2xx fast. 429 when overloaded, never a redirect.
2. **`GET /commands`** — per-host queue, strict output, with the lifecycle above.
3. **Storage** — events per host and per container; `logs` under your log-data access rules.
4. **UI** — a fleet view (one row per host, last-seen from the newest event, so a silent
   Bosun is visible), a container view (current version, last update, downtime, backup size
   and age, what is on the skip list), and a failure view: the `steps` list rendered in order
   with the failing step marked, plus the `logs` tail beside it. That pairing is the whole
   reason this link exists.
5. **Two buttons** — "check now" and "allow this version again", which enqueue `check` and
   `skip_clear` and then show pending → done, driven by `command.result`.
6. **Alerts** — `gate.refused` loudly, `update.rolled_back` and `registry.failing` normally.

## Rules for you

- Follow whatever process this project already uses (brainstorm → spec → plan, if that is the
  pattern in its repo).
- Test against the real thing: Bosun is a Go binary in the repo above, and
  `internal/control/` has the exact client. A fake Sentinel in a test is fine, but at least
  once, run Bosun against your endpoints for real.
- Bosun's side is done. If you need a change there, say so and why; do not work around it with
  something Sentinel-side that guesses.
