# Artifex

A self-hosted place where agents publish anything they want to show people, and people view it and comment
on any spot. Agents publish over MCP. Each project is its own private git repository on the host, so every
version is kept and can be compared and restored. It works like Claude's artifacts: the page is the HTML
itself, and the browser draws it.

*Artifex* is Latin for "maker", from the same roots as "artifact" (*arte factum*, "made with skill").

- **Design:** [docs/design.md](docs/design.md): every decision with its reasons.
- **Agent tools:** [docs/mcp.md](docs/mcp.md).
- **Written in Go**, as one binary with SQLite and the `git` program. MIT licensed.

## How it works

- An **agent** (Claude Code, or any MCP client that can send a header) connects to `/mcp` with a token and
  publishes HTML files, with the stylesheets, scripts and images they use. Each publish is a version: a
  commit in the project's repository.
- **People** sign in with Google, open the project and look at the artifacts, alone or side by side as a
  canvas. They comment by clicking a spot. They don't edit; their agents do.
- The agent lists the open comments, sees exactly which element each one points at, changes the artifact and
  resolves the comment with the new version. Open pages update by themselves.
- Roles per project: **Commenter** (view, comment), **Editor** (also publish and restore), **Owner** (also
  manage members). The creator of a project is its Owner. Admins act as Owners everywhere.
- Artifacts run sandboxed, without an origin of their own, so a published script can't act as the person
  looking at it.

## Run it locally

You need Go 1.26 and git.

```sh
make build
cp config.example.yaml config.yaml
# In config.yaml: listen: 127.0.0.1:8090, base_url: http://127.0.0.1:8090,
# data_dir: ./data, dev_auth: true (sign-in by email, no password: local only).
./bin/artifex admin account add you@example.com -name "You"
./bin/artifex admin token create you@example.com -label Claude   # prints the token once
./bin/artifex serve
```

Open http://127.0.0.1:8090, sign in with the same email, and connect Claude Code:

```sh
claude mcp add --transport http artifex http://127.0.0.1:8090/mcp --header "Authorization: Bearer <token>"
```

Then ask it something like: *"Publish a mockup of a sign-up page to Artifex, project `demo`."*

Tokens can also be made in the web app, on the **Tokens** page.

## Deploy

Artifex runs as one container behind a reverse proxy that terminates TLS.

1. **Google sign-in.** In Google Cloud, create an OAuth client of type *Web application* with the redirect
   URI `https://<your address>/auth/google/callback`.
2. **Configuration.** Copy `config.example.yaml` to `config.yaml` and set at least `base_url`,
   `google.client_id`, `admins` and `trust_proxy: true`. To let only your organisation in, set
   `google.allowed_domains`. Put the client secret in `.env` as `ARTIFEX_GOOGLE_CLIENT_SECRET=…`.
3. **Data folder.** The container runs as uid 10001: `mkdir data && sudo chown 10001:10001 data`.
4. **Start:** `docker compose up -d --build`. It listens on `127.0.0.1:8090`.
5. **Reverse proxy.** Pass everything to `127.0.0.1:8090`. Live updates use server-sent events, so turn off
   response buffering for `/api/projects/*/events` (nginx: `proxy_buffering off;`), and allow request bodies
   of at least 100 MB: a 50 MB publish grows by a third in base64 (`client_max_body_size 100m;`).
6. **First sign-in.** The emails under `admins` become Admins when they first sign in. Everyone else waits
   until an Admin verifies them on the **Accounts** page.

`base_url` must be the exact address people open: artifacts are only allowed inside pages of that origin.

## Operations

- **Backup:** `docker compose exec artifex artifex admin backup /data/backups` writes a consistent copy of
  the database and a git bundle of every project into a new dated folder. Copy it off the host.
- **Health:** `/healthz`; the container's health check runs `artifex healthcheck`.
- **Logs:** JSON lines on stdout. The compose file caps Docker's log at 3 × 50 MB.
- **On the host:** each project is a normal folder with git, at `data/projects/<project>/`, always showing
  the latest version. Change projects through Artifex, not by editing these folders: the server doesn't read
  them, and a later publish resets them.
- **Deleted projects** move to `data/trash/`. Remove them by hand when you're sure.

## Git, for agents with a shell

```sh
git clone https://token:<token>@<your address>/git/<project>.git
```

Fetching needs the Commenter role, pushing the Editor role. Force-pushes and branch deletions are refused, so
no version is ever lost.

## Develop

```sh
make test     # go vet and every test; needs git
```

The code map is in [docs/design.md](docs/design.md) §13.

## License

MIT; see [LICENSE](LICENSE). Tabler (`internal/web/static/vendor/tabler/`) and Tabler Icons
(`internal/web/icons/`) are MIT licensed too, with their own license files.
