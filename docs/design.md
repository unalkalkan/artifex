# Artifex — design

*Artifex* is Latin for "maker", from the same roots as "artifact" (*arte factum*, "made with skill").

**Goal:** a self-hosted service where agents publish anything they want to show people, and people view it
and comment on any spot.

- Every project is its own private git repository, with its own members, so every version is kept.
- It works like Claude's artifacts: the published page is the HTML itself, and the browser draws it.

**How to read this:** §1 lists the requirements and how they are met. §2 holds every decision, each with its
reasons, what was rejected, and what changes if it is reversed. §3–§8 describe the system as built, §9–§11
what it doesn't do, §12 how it was built and checked, and §13 where things are in the code.

---

## Terms

| Term | Meaning |
|---|---|
| **project** | A private git repository that Artifex hosts, with its own members and roles. It holds artifacts and the files they use |
| **artifact** | An HTML file in a project, shown to people: a mockup screen, a report, a diagram, an explainer |
| **asset** | Any other file in a project that artifacts use: stylesheets, fonts, images, scripts, data |
| **folder** | A folder in a project. Its artifacts can be shown together as a canvas |
| **canvas** | A folder's artifacts shown one under another, each at its width and under its title |
| **label** | A word that a folder puts on all its artifacts, such as "Proposal" (decision 12) |
| **version** | One commit of a project. An artifact *at a version* is its file, plus the project's other files, as of that commit |
| **publish** | One change to a project by an agent: files written, moved or deleted together, as one commit |
| **thread** | A comment and its replies, pinned to a spot on an artifact at one version |
| **pin** | The spot a thread points to: an element of the artifact, and a point on it (decision 14) |
| **account** | A person who signs in with Google. Every account is a **Member** or an **Admin** |
| **project role** | What an account may do in one project: **Owner**, **Editor** or **Commenter** (decision 16) |
| **agent** | A program that uses an account's token over MCP. Usually Claude Code |
| **bridge** | The small script the server adds to every artifact it serves (§4.4) |

In mockup terms, a **board** is an artifact, and a **page** of boards is a folder.

---

## 1. Requirements, and how they are met

Artifex started from one use, a home for HTML mockups that agents write, and was widened to anything an
agent wants to show (decision 5). Everything below holds for mockups too.

| Requirement | How |
|---|---|
| 1. Open source, self-hosted, Docker | A Go service in one container, behind a reverse proxy (decision 18) |
| 2. Full agent access with a token, and no per-call or per-seat limits | MCP tools, authenticated by a token (decisions 9 and 17, §7) |
| 3. Exact HTML fidelity | An artifact is the HTML itself, drawn by the browser (decision 1) |
| 4. Named boards in pages, with links between boards | Artifacts in folders, shown as a canvas, and linked with ordinary relative links (decisions 12 and 13) |
| 5. People can view, comment on a spot and edit; roles; sharing by link | People view and comment in the browser. Agents edit (decision 2). Project roles decide who may do what (decision 16). Sharing is adding a member and sending the address |
| 6. Version history and restore | Git: every publish is a commit. Any version can be viewed and compared, and restored by an agent or by an Editor (§6) |

**Nice-to-haves:**

- **Met:**
  - text files in git that can be diffed;
  - Google sign-in;
  - a change feed: the threads changed since a given time (§7);
  - an MCP server with write tools, as the main interface (decision 9);
  - export: an artifact is already an HTML file;
  - low upkeep: one container, SQLite, and the project repositories on one volume.
- **Partly met:** reusable components and design tokens. A project's own stylesheets play that role.
- **Not met:** turning HTML into editable design layers, real-time co-editing, and webhooks (§11).

---

## 2. Decisions

### Decision 1 — What people see is HTML, drawn by the browser

- **Decision:** artifacts are HTML documents, shown as they are. Nothing translates them into another drawing
  model.
- **Why:**
  - Design tools draw their own model, so HTML has to be translated into it, and the translation loses
    things without saying so: icons drawn with CSS masks, text inside form controls, uppercase labels,
    spacing in monospace text, ellipses, scrollable areas. None of these raises an error; each is found by
    comparing screenshots, and every new CSS feature a page uses needs a fix of its own.
  - Drawing the HTML in a browser has none of these problems.
  - Agents write HTML well. It is what they produce when asked for a mockup, a report or a diagram.
- **Rejected:**
  - **Penpot:** it can't show real HTML.
  - **Storybook:** its open-source version has no comments, roles or history, and it keeps HTML as copies
    inside JavaScript.
  - Any design editor that imports HTML, for the translation losses above.
- **If reversed:** the translation step comes back, and with it the silent losses.

### Decision 2 — People comment, agents edit

- **Decision:**
  - In the browser, people view artifacts and comment on them. The web app has no editor.
  - Only agents write files.
  - **The one exception:** a person with the Editor or Owner role can restore an earlier version from the
    browser (§6.3). A restore publishes the old content as a new commit, so nothing is lost and the history
    shows who did it.
- **Why:**
  - Every change comes in one way, with the same publish step, the same history and the same attribution.
  - A person and an agent never edit the same file at the same time, so their edits never need merging.
  - A restore writes no new content, only content the project already had. So it needs no editor, and it
    can't conflict with anything (decision 7).
- **Costs:**
  - A change someone asks for waits for an agent to run.
  - Nobody can sketch freehand in Artifex.
- **This is a product rule, not a security boundary.** A member's token can publish with that member's role
  (decision 17).
- **If reversed:** an editor in the browser would have to handle a person and an agent editing the same file.
  The history would also have to show both kinds of edit.

### Decision 3 — Built, because no existing product fit

- **Decision:** Artifex is its own service, not an existing product adopted or extended.
- **Why:** existing self-hostable products were researched first: design tools, component workshops and
  review tools. None met the requirements of §1 together. Most can't show real HTML exactly as a browser
  draws it (decision 1). The rest lack one of the others: full agent access with a token and no per-call or
  per-seat limits, comments pinned to a spot, roles per project, or a complete history with restore.
- **If reversed:** adopting a product means giving up whichever of those requirements it lacks.

### Decision 4 — Go

- **Why:** one static binary that runs in a small container, a standard library that covers HTTP, templates
  and CGI (for `git http-backend`), and simple concurrency for the per-project locks.
- **Libraries:** `mattn/go-sqlite3`, `zap`, `yaml.v3`, `golang.org/x/oauth2` and `golang.org/x/time`, plus
  the official MCP SDK, `github.com/modelcontextprotocol/go-sdk` v1.8.0. Each was the newest release at least
  two weeks old when it was added.

### Decision 5 — For anything an agent wants to show

- **Decision:**
  - Nothing in the system is specific to mockups.
  - A project holds any HTML artifacts and the files they use.
  - Ideas that belong to one kind of project stay in that project: as-is and proposal screens, boards grouped
    into pages, fixed widths. A project expresses them with folders, labels and its own artifacts.
- **Why:** agents make many kinds of pages for people: mockups, reports, diagrams, explainers. Mockups are one
  kind.
- **Consequences:**
  - The names are generic: artifact, folder, canvas.
  - An artifact without a width fills the window like an ordinary web page (decision 12).
  - Only HTML files are artifacts, with comments. Any other file is still served as it is, so images, PDFs
    and text open in the browser.

### Decision 6 — Each project is its own private git repository, hosted by Artifex

- **Decision:**
  - Each project is a folder with git initialised, on the host's filesystem at `data/projects/<project>/`. It
    is not connected to GitHub or any other remote.
  - The folder's files are kept equal to the latest version, so anyone looking at the host sees the current
    files. Artifex itself never reads them: every read names a commit and comes from git's objects.
  - An agent creates a project by uploading to it (`upload_artifact`) or with `create_project`. The creator
    becomes the project's Owner.
  - Accounts, project roles, tokens and threads live in Artifex's SQLite database.
- **Why:**
  - A project is private to its members (decision 16). Artifex can only enforce that if it holds the
    repository.
  - An agent that has MCP and no shell can create a project and publish to it (decision 9).
  - Each project's history, diff and restore come from git.
  - Creating a project needs no outside service.
- **Rejected:**
  - **One repository for all projects:** members of one project could read another project's history, and
    permissions would have to be set per folder.
  - **Repositories on GitHub:** Artifex would need admin rights in an organisation to create them, and access
    to a project would be decided in two places.
- **Costs:** backups are the operator's job (§10).
- **If reversed:** the repositories move elsewhere, and project access has to be kept in step with wherever
  they go.

### Decision 6b — Every version stays: pushes can't rewrite history

- **Decision:** each repository is created with `receive.denyNonFastForwards` and `receive.denyDeletes`, and
  `receive.denyCurrentBranch updateInstead` so a push also updates the folder's files. Pushes are also
  checked object by object (`receive.fsckObjects`) and may send at most four publishes' worth of data
  (`receive.maxInputSize`).
- **Not checked on a push:** the per-file size limit and the path rules of `publish`. A pushed file in a
  top-level folder named `-` can't be opened in the web app.
- **Why:** the history is what makes restore possible (§6). A force-push or a deleted branch would remove
  versions, and threads that point at them, with no way back. The repository refuses both, whatever the
  client does.
- **If reversed:** a push could drop versions that threads and comparisons point at.

### Decision 7 — One publish is one commit, checked file by file

- **Decision:**
  - The MCP tool `publish` writes, moves and deletes files in a single commit.
  - Each file it changes names the version of that file the agent started from: its `base`, the blob id that
    `read_file` returned. A new file has no base.
  - If any of those files has changed since its base, nothing is written. The answer carries those files'
    current content, so the agent can merge and publish again.
  - Files the agent didn't touch may change meanwhile, without a conflict.
- **Why:**
  - Two agents can't silently overwrite each other's work.
  - Agents working on different artifacts of the same project don't block each other.
  - It is the same shape as Claude's artifact publish: start from a version, get a conflict if it moved,
    merge.
- **Rejected:**
  - **One base for the whole project:** agents working on different artifacts would conflict all the time.
  - **Last write wins:** the overwritten work is lost without a sign.
- **Details:**
  - A base may be shortened to 7 characters or more.
  - Writing a file that already exists without a base is a conflict too: the agent may not know it is there.
  - A publish that changes nothing creates no version and says `unchanged`.
  - The answer lists each file's new blob id, so an agent can change it again without reading it first.

### Decision 7b — A publish records its moves

- **Decision:** when a publish moves a file, the commit message gets a line
  `Artifex-Moved: "<old path>" "<new path>"`. Following a thread to where its artifact is now (§5.3) reads
  these lines before it asks git.
- **Why:** git only pairs a rename with its edit when the content stayed similar. An agent often moves and
  rewrites an artifact in one publish, and git then sees a deletion and a new file, so the thread would be
  cut off. The publish knows the move exactly, so it writes it down.
- **Rejected:** a stable id written inside each artifact (§5.3).
- **If reversed:** threads lose their artifact when it is moved and rewritten in the same publish. Moves made
  by a git push still depend on git's rename detection.

### Decision 8 — Git through the `git` program

- **Decision:**
  - The server runs `git` plumbing commands: `hash-object`, a temporary index, `commit-tree`, and
    `update-ref <branch> <new> <old>`.
  - The container includes git.
- **Why:**
  - `update-ref` with the old value moves the branch only if nobody has moved it since. Two publishes at the
    same moment can't lose a commit; git guarantees it. The loser of the race checks its files again
    (decision 7) and retries.
  - Rename detection, history by path and diff are git's own, and complete.
  - `git http-backend` gives git over HTTPS without writing a git server.
- **Rejected:** **go-git**, a pure-Go library. It needs no program in the container. But history by path walks
  every commit, which is slow on long histories, and git over HTTPS would have to be our own code.
- **Costs:** one process per git operation. That is fine at this scale.

### Decision 9 — Agents use MCP

- **Decision:**
  - The agent interface is an MCP server at `/mcp`, over the Streamable HTTP transport. It authenticates with
    bearer tokens (decision 17). §7 lists the tools.
  - Agents get no separate CLI. The binary's subcommands are for running the server (`serve`, `admin`).
  - Git over HTTPS is a second, optional way in, for agents with a shell:
    `https://token:<token>@<host>/git/<project>.git`. Fetching needs the Commenter role and pushing the Editor
    role.
- **Why:**
  - MCP is the interface agents already speak, and it works for agents with no shell.
  - Claude Code connects with
    `claude mcp add --transport http artifex https://<address>/mcp --header "Authorization: Bearer <token>"`.
- **Not built yet:** OAuth sign-in for MCP. The connectors in claude.ai and Claude Desktop sign in with OAuth,
  not with a token header, so they can't connect yet (§9).
- **As built:** the server is stateless (each request stands alone, no MCP session) and answers with JSON.
  Its built-in DNS-rebinding guard is off: behind a reverse proxy every request arrives from localhost with
  the public host name, which the guard would refuse, and every request needs a token anyway.

### Decision 10 — Artifacts run isolated, at signed addresses

- **Decision:**
  - The app shows each artifact in an iframe with
    `sandbox="allow-scripts allow-popups allow-popups-to-escape-sandbox"` and without `allow-same-origin`.
    The artifact then runs without an origin of its own (an "opaque" origin).
  - Every content response also carries the same `sandbox` in its Content-Security-Policy. So an artifact
    opened directly in a tab, outside the iframe, still has no origin.
  - A popup may leave the sandbox, so an external link opens as a normal page; Artifex content opened that
    way is sandboxed again by its own CSP. No modals (`prompt()` could imitate a sign-in dialog) and no
    downloads (they would appear to come from the Artifex host).
  - The app's own pages run scripts only from `/static/`, not from anywhere on the origin: artifact files are
    served from the same origin, under `/c/`.
  - The bridge's "open" message may only name a file of the project; the page refuses `..`, other app pages
    and anything but a plain `#anchor`.
  - The page follows "open" only right after the reader clicked or pressed a key inside that artifact: the
    artifact's frame has focus, and the browser reports user activation (`navigator.userActivation`), which
    a click in a frame also gives the page and which a script can't fake. Links still work, because the
    bridge sends them from the click itself. Without this check, an artifact could move the reader the moment
    it loads, and on a canvas any one artifact could take the reader off the page.
  - An artifact's address carries a short-lived signature and the version:
    `/c/<signature>/<project>/<commit>/<path>`.
  - The artifact's relative links to assets resolve under the same prefix. So the assets are signed too, and
    pinned to the same commit, with no extra work.
- **Why:**
  - Artifacts may run scripts (decision 20), so an artifact must not be able to read the viewer's session or
    call the API as the viewer.
  - Without an origin, the browser itself prevents that. The artifact can't read the app's cookies or
    storage, its requests don't carry the session cookie, and CORS hides the responses from it.
  - That holds for every artifact anyone publishes, not only for ones someone has reviewed.
  - Claude's artifacts are separated from claude.ai the same way: they run on a different origin.
- **Costs:**
  - The signature protects the content, not the session. A leaked address works until it expires: after 2
    hours by default (`limits.content_url_ttl`). A few minutes would be safer, but an artifact that loads a
    file later, such as a chart reading `data.json` when someone clicks, needs the address to still work.
    Open pages fetch fresh addresses whenever a new version arrives.
  - Fonts and module scripts load in CORS mode, so content responses carry
    `Access-Control-Allow-Origin: *`.
  - Artifacts can't use cookies or `localStorage`.
- **Rejected:**
  - **Iframes on the app's own origin:** any artifact's script could act as the viewer.
  - **A second hostname for content**, as Claude uses: just as safe, but it needs another DNS name and
    certificate. It is the fallback if a browser quirk gets in the way.
- **If reversed:** every artifact's scripts would have to be trusted.

### Decision 11 — Artifacts use only their own project's files

- **Decision:**
  - Artifacts load files only from their own project, plus `data:` and `blob:` URLs.
  - A Content-Security-Policy on every artifact allows no other host.
  - Libraries and fonts are copied into the project, not linked. A mockup styled with a CSS framework carries
    a copy of that framework, at the version the product it shows uses.
- **Why:**
  - An artifact keeps looking the way it did when it was published. A CDN copy can change or disappear, and
    a mockup of an existing screen is only exact with that screen's exact CSS.
  - Opening an artifact tells no third party who looked at it.
- **Costs:** agents copy libraries in instead of linking them. Pages written to load from a CDN have to be
  rewritten to use the copies.
- **If reversed:** artifacts break when a CDN changes or can't be reached.

### Decision 12 — Each artifact describes itself; a folder sets order and labels

- **Decision:**
  - An artifact's title is its `<title>`, or its file name when the tag is missing.
  - `<meta name="artifact" content="width=1440">` gives it a fixed width, as a mockup screen needs. Without
    the tag it fills the window like an ordinary web page, and in a canvas it is shown 1440 wide.
  - Its height follows its content.
  - A folder can have a `folder.yaml` with a title, the order of its artifacts and a label. It is optional.
  - A project can have a `project.yaml` with a title and the artifact to open first; the default is
    `index.html`.
- **Why:**
  - Everything about an artifact is in one file, so nothing elsewhere can disagree with it.
  - The YAML files only set order and wording. An artifact they don't list still shows, after the listed
    ones. A listed artifact that doesn't exist only produces a warning in the publish answer.
  - **A label belongs to the folder, not to the artifact.** A mockups project can put proposals in folders
    labelled "Proposal". A proposal can then never sit among the screens that show what exists, because the
    folder decides what its artifacts are. To promote a proposal, an agent moves its file into another
    folder.

### Decision 13 — Links between artifacts are ordinary relative links

- **Decision:**
  - An artifact links to another one with a relative path to its file, such as
    `<a href="../settings/billing.html">`.
  - The bridge (§4.4) turns a click on such a link into opening that artifact in the app.
  - Links to anywhere else open in a new tab.
- **Why:**
  - The links work when an artifact is opened from disk, and in diffs.
  - They are plain paths that an agent can follow and check. The publish answer lists broken ones.
- **Rejected:** a link scheme of our own, such as `artifact:settings/billing`. It would work nowhere else.

### Decision 14 — Threads are pinned to an element and a point

- **Decision:**
  - A thread records the artifact, the version it was made on, an element and a point.
    - The element is a CSS selector path plus the element's text, shortened.
    - The point is x and y in the artifact's own pixels, at the width it was shown at.
  - On later versions, the pin finds the element by its selector and text. If it can't, it uses the point and
    marks the thread "may have moved".
- **Why:**
  - The element moves with the thing the person meant when the layout around it changes.
  - It also tells an agent exactly what was meant. The agent reads the selector and the HTML around it,
    instead of guessing what sits at a point.
- **Rejected:**
  - **A point only:** it drifts with every layout change and leaves the agent guessing.
  - **An element only:** it can't hold a comment on empty space, such as "put a filter here".

### Decision 15 — Google sign-in; Member and Admin accounts

- **Decision:**
  - People sign in with Google. `google.allowed_domains` can limit sign-in to an organisation's Google
    Workspace domains.
  - A new account waits until an Admin verifies it. The emails listed under `admins` are Admins from their
    first sign-in.
  - A **Member** can sign in, create projects, make their own tokens, and act in the projects they belong to.
  - An **Admin** can also verify, disable and remove accounts and change account roles, but never on their own
    account: no Admin can lock themselves out, and a change to an Admin's account is always made by someone
    else.
  - There is no view-only account and no access without an account.
  - For local use, `dev_auth` signs anyone in by email with no password.
- **Why:** with no editing by people (decision 2), the accounts only answer two questions: who may sign in,
  and who manages the people. What someone may do inside a project is decided per project (decision 16).
- **The domain check uses Google's Workspace claim (`hd`), not the address.** A personal Google account can
  carry an organisation's address, made before the organisation used Workspace; with `allowed_domains` set, it
  is refused.

### Decision 16 — Project roles: Owner, Editor, Commenter

- **Decision:**

  | Role | May |
  |---|---|
  | **Commenter** | View the project's artifacts and history. Comment, reply, resolve and reopen threads |
  | **Editor** | All of that, plus publish through an agent and restore earlier versions |
  | **Owner** | All of that, plus add and remove members, change their roles, rename the project and delete it |

  - Only members of a project can see it, and it doesn't appear in anyone else's project list.
  - An Owner adds a member by email. If that email has no account yet, the entry waits and takes effect once
    the account is verified.
  - A project always has an Owner: the last Owner can't leave or be demoted, and an account that is the only
    Owner of a project can't be removed until the project has another Owner.
  - **Admins act as Owners of every project**, so a project whose Owners have left can be recovered. They see
    the projects they don't belong to in their own list on the home page.
- **Why:**
  - Projects are private (decision 6), so access is decided per project.
  - Restoring changes the project, so it needs a role above commenting.
  - There is no view-only role: people only view and comment (decision 2), so the least a member does is
    comment, and a role that differs only by not commenting isn't worth its place.
- **Rejected:** roles on the account instead of per project. Someone who edits one project would then be an
  Editor in every project they join.

### Decision 17 — Tokens: any member makes their own

- **Decision:**
  - Any Member or Admin makes tokens for themselves, on the Tokens page.
  - A token acts as its account, with that account's project roles. A Commenter's agent can read and comment,
    but not publish.
  - A thread or reply posted with a token shows as "Claude for \<name\>"; the token's label sets the first
    part.
  - Tokens are shown once and stored hashed. Their owner, or an Admin, can revoke them.
  - On the server, `artifex admin token create <email> <label>` makes one too, for setting up a fresh
    install.
- **Why:** every change and every reply can be traced to the person who ran the agent.

### Decision 18 — Behind a reverse proxy, at an address from configuration

- **Decision:**
  - The public address comes from configuration (`base_url`).
  - The container serves plain HTTP, and a reverse proxy in front of it handles TLS.
  - Sign-in, MCP, git and the pages' own API calls are rate-limited per client address, with
    `golang.org/x/time/rate`: 300 a minute by default.
- **Why:** TLS, certificates and host names are the proxy's job on most servers, so Artifex doesn't duplicate
  them.
- **Consequences:**
  - Everything reachable from outside needs a sign-in, a token or a signed address. The only exception is
    `/healthz`.
  - Decisions 10 and 11 are requirements, not nice-to-haves.

### Decision 19 — No notifications

- **Decision:** nobody is notified of new threads or replies. People see them in the app, and agents through
  MCP (`list_threads` with `since`).
- **If reversed:** a notification channel, such as email or a chat webhook, is added later. Nothing in this
  design blocks it.

### Decision 20 — Artifacts may contain scripts

- **Decision:** artifacts may run JavaScript, loaded from their own project (decision 11).
- **Why:** reports need charts, and mockups need tabs and menus. Isolation (decision 10) makes it safe.

### Decision 21 — Open views update themselves

- **Decision:** when a new version of an artifact is published, any view that has it open reloads it, and new
  comments appear without a reload. Both arrive through server-sent events. The stream checks the reader's
  membership before every event, so someone removed from a project stops receiving them.
- **Why:** the usual loop is that a person comments and an agent answers within minutes. The person shouldn't
  have to reload to see the answer. Claude's artifacts update open views the same way.

### Decision 22 — The app's look: Tabler, dark slate

- **Decision:** the app's own pages use Tabler `1.0.0-beta20` in a dark slate theme: the body classes
  `theme-dark theme-slate` with `data-bs-theme="dark"`, and slate colours defined in `static/app.css`. The app
  is dark only.
- **Why:**
  - Tabler is a complete, MIT-licensed kit built on Bootstrap: navigation, cards, lists, forms, badges and
    dialogs. It needs no build step, so the pages stay plain Go templates.
  - A dark frame keeps attention on the artifacts, which bring their own colours.
- **How:**
  - **Version:** beta20 has no slate colours of its own; `static/app.css` defines them (the slate scale for
    Tabler's greys, and the dark surfaces built on it). Tabler 1.4 ships its own slate base and could
    replace both in one change.
  - **Served by Artifex, not loaded from a CDN.** Tabler's CSS and script are in
    `internal/web/static/vendor/tabler/`, copied unchanged from the npm package and checked against its
    published sha512. The app's CSP allows scripts and styles only from `/static/` (decision 10); loading from
    a CDN would mean trusting an outside host there, and the app would break whenever the CDN is down.
  - **Icons:** outline icons from `@tabler/icons` 3.49, drawn inline by the `icon` template function. No
    requests, nothing to cache. An unknown icon name fails the page, so the tests catch a typo.
  - **Navigation along the top,** not in a sidebar. Artifacts need the width: on a 1440-pixel laptop, a
    sidebar would shrink a 1440-pixel mockup from about three quarters of its size to about half.
  - **Readable blue:** Tabler's primary blue (#0054a6) is too dark to read on slate. Where it would be read
    (the active tab and navigation, blue badges), text is white with a #3b82f6 line, and badges are azure.
    Buttons keep the primary colour.
  - **Static files are gzipped and revalidated by ETag:** Tabler's CSS is 550 KB, 69 KB gzipped.
- **Rejected:**
  - Loading Tabler from a CDN: see above.
  - A light theme alongside the dark one: every colour would have to be checked twice.
- **If reversed:** every template uses Tabler's classes, so leaving Tabler means rewriting all of them and
  `app.css`. Artifacts are not affected: they bring their own styles (decision 11).

---

## 3. A project's repository

### 3.1 Layout

Nothing about the layout is required. These are two examples.

Mockups of a product's screens:

```
app-mockups/
  project.yaml                     title; the artifact to open first
  assets/
    tabler-1.0.0-beta20/           a CSS framework, vendored unchanged
    app.css                        the product's own stylesheet
    icons/  fonts/  images/
  settings/
    folder.yaml                    title, order
    profile.html                   an artifact, one screen
    billing.html
    billing-loading.html
  proposals/
    folder.yaml                    label: Proposal
    team-invites.html
```

A report:

```
weekly-kpis/
  index.html
  charts.js
  data.json
```

### 3.2 The YAML files

```yaml
# settings/folder.yaml
title: Settings
label: ""            # e.g. Proposal; shown on every artifact in this folder
order:               # order only; artifacts not listed here come after these
  - profile
  - billing
  - billing-loading
```

### 3.3 The head of a mockup artifact

```html
<!doctype html>
<html lang="en">
<head>
  <title>Billing</title>
  <meta name="artifact" content="width=1440">
  <link rel="stylesheet" href="../assets/tabler-1.0.0-beta20/css/tabler.min.css">
  <link rel="stylesheet" href="../assets/app.css">
</head>
<body>
  …
```

---

## 4. Showing artifacts

### 4.1 Projects

The app's home page lists the projects the person belongs to, with their role in each.

### 4.2 An artifact

An artifact opens at its width, or at the window's width when it has none. Its threads and its versions
(§6) appear beside it.

### 4.3 A canvas

- A folder's artifacts are shown one under another, each at its width and under its title.
- You can zoom to fit, or view at 100%.
- Each iframe loads when it scrolls into view, so a folder with many artifacts opens quickly.

### 4.4 The bridge

The server adds one script to every artifact it serves. The files in the repository don't contain it. The
script talks to the app through `postMessage`:

- **Size:** it reports the artifact's height, so the iframe fits the content. The page scrolls, not the
  artifact.
- **Comment mode:** it highlights the element under the pointer. On a click, it sends that element's selector,
  its text and the point.
- **Pins:** it draws pins at the positions the app sends it.
- **Links:** it turns a click on a link to another artifact into an "open artifact" message.

The app accepts messages only from its own artifact iframes.

### 4.5 Addresses

- **In the app:**
  - `/p/<project>` for a project;
  - `/p/<project>/<path>` for an artifact, or for a folder, which opens as a canvas;
  - `?v=<commit>` added for a version, and `#t<thread>` for a thread.
- **Content:** `/c/<signature>/<project>/<commit>/<path>`. The app makes these for each view (decision 10).

---

## 5. Comments

### 5.1 A thread

A thread holds:

- the project, the artifact's path, and the commit the thread was made on;
- the pin's element (selector and text) and point, and the width the artifact was shown at;
- the author, the body and the replies;
- its state, open or resolved, and the commit it was resolved at.

Agents can start threads too. For example, "Is this the order you meant?"

### 5.2 Life of a thread

- A thread opens, and collects replies.
- Any member of the project can resolve it or reopen it.
- When an agent resolves a thread after changing the artifact, it records the version with the change. The
  person can then see before and after side by side (§6.2).

### 5.3 Following an artifact through renames

- A thread keeps the path it was made on.
- When an artifact is moved or renamed, the server follows it with the publish's recorded moves (decision 7b)
  and git's rename detection, so the thread stays with its artifact.
- **Rejected:** a stable id written inside each artifact. Every agent would have to keep it intact, and one
  that didn't would cut the artifact's threads off without any error.
- **Cost:** what each commit did to paths is cached by commit id, since a commit never changes. A listing
  follows each thread with map lookups once those are known, not with git runs per thread.

### 5.4 What an agent reads

`get_thread` returns:

- the comment and its replies;
- the selector, the element's text and the point;
- the artifact's HTML around the element, at the thread's commit;
- whether the artifact has changed since.

---

## 6. Versions

### 6.1 History

- A project's history lists its commits.
- An artifact's history lists the commits that changed its own file, following renames. It doesn't count
  changes to the files the artifact uses: every file is one that some artifact may use, so that history would
  be the project's history again. A shared stylesheet's change shows in the project's history.
- Each version shows its message and its author. The author is the account, and the token's label for an
  agent.

### 6.2 Comparing

- Two versions side by side, as drawn.
- The source diff between them.

### 6.3 Restoring

- **An agent** uses the MCP tool `restore`.
- **A person** with the Editor or Owner role uses the Restore button in the browser.
- Either way, the old content is published as a new commit. Restoring one artifact brings back its own file
  only; restoring a whole project brings back every file.

---

## 7. The MCP tools

Each tool checks the token's project role (decision 16). `docs/mcp.md` has every argument; the server also
gives agents a short guide in its MCP instructions.

| Tool | Role | What it does |
|---|---|---|
| `list_projects` | any | The projects the account belongs to, with its role in each |
| `create_project(project, title)` | any account | Creates the repository. The caller becomes its Owner |
| `upload_artifact(project, path, content, base, …)` | Editor | Writes one file as a version. Creates the project when it doesn't exist; the caller becomes its Owner |
| `list_files(project, path?, version?)` | Commenter | Files and folders, with each file's blob id |
| `read_file(project, path, version?)` | Commenter | One file's content and its blob id, which is the `base` for `publish` |
| `publish(project, message, changes)` | Editor | Writes, moves and deletes files as one commit (decision 7). Returns the version, the addresses of changed artifacts, and warnings |
| `history(project, path?, limit?)` | Commenter | Versions, newest first |
| `diff(project, from, to, path?)` | Commenter | The source diff between two versions |
| `restore(project, version, path?)` | Editor | Publishes the old content as a new commit (§6.3) |
| `get_url(project, path, version?)` | Commenter | The app address of an artifact or folder |
| `list_members(project)` | Commenter | Members and their roles |
| `set_member(project, email, role)` / `remove_member(project, email)` | Owner | Manages members (decision 16) |
| `list_threads(project?, state?, since?)` | Commenter | Threads across the account's projects. With `since`, this is the change feed |
| `get_thread(id)` | Commenter | One thread (§5.4) |
| `create_thread(project, path, anchor, body)` | Commenter | Starts a thread |
| `reply(id, body)` / `resolve_thread(id, version?)` / `reopen_thread(id)` | Commenter | Answers and closes threads |

**What a change in `publish` looks like:**

```json
{ "path": "billing.html", "content": "<!doctype html>…", "base": "3f2a…" }
{ "path": "fonts/inter.woff2", "content_base64": "…" }
{ "path": "old/name.html", "move_to": "new/name.html", "base": "9c1e…" }
{ "path": "draft.html", "delete": true, "base": "77b0…" }
```

- Binary files are sent as base64.
- One publish is limited in size; the limit comes from configuration. `/mcp` accepts requests a third
  larger plus 1 MiB, so a publish at the limit fits in base64 (`Limits.MaxRequestBytes`, used for both the
  HTTP limit and the MCP SDK's own, whose 4 MiB default refused larger publishes).

---

## 8. Access and security

- **People:**
  - HttpOnly session cookies with `SameSite=Lax`. Only a hash of the session id is stored.
  - A CSRF token on every form, and on every request that changes something, plus an `Origin` check.
  - The app's own pages carry a strict CSP: scripts only from the app, no inline scripts, no framing.
- **Agents:** bearer tokens on `/mcp`. Tokens are stored hashed.
- **Content:** never served on the strength of the session cookie. Only signed addresses can fetch it, and it
  runs isolated (decision 10).
- **Rate limits:** on sign-in, MCP, git and the pages' API (decision 18).
- **Paths:** every path a tool receives is cleaned and kept inside its project. `..`, absolute paths and
  `.git` are refused.

---

## 9. Not built yet

- **OAuth for MCP clients.** The connectors in claude.ai and Claude Desktop sign in with OAuth, so they can't
  connect until Artifex offers it. Claude Code and any client that sends a token header work now
  (decision 9).
- **Notifications** (decision 19).

---

## 10. Running it

- **Container:** one, with git installed (decision 8). It listens on plain HTTP behind a reverse proxy
  (decision 18). `Dockerfile` and `compose.yaml` are in the repository; `README.md` has the steps.
- **Volume:**
  - `data/app.db`: SQLite, holding accounts, project roles, tokens and threads;
  - `data/projects/<project>/`: one folder with git initialised per project (decision 6);
  - `data/trash/`: deleted projects' folders, kept until someone removes them by hand;
  - `data/signing.key`: made on first start when no key is configured.
- **Configuration** (`config.example.yaml` describes every key):
  - the public address;
  - the Google OAuth client and the allowed domains;
  - the key that signs content addresses;
  - limits: file and publish size, address lifetime, session lifetime, rate limits.
- **Backups:** `artifex admin backup <dir>` writes a consistent copy of the SQLite file (`VACUUM INTO`) and a
  `git bundle` of each project into a new dated folder. Copying it off the host is up to the operator.
- **Logs:** structured, with `zap`, to stdout.
- **Health check:** `/healthz`, and `artifex healthcheck` for the container.

---

## 11. Not in scope

- Editing or drawing in the browser (decision 2).
- Real-time co-editing.
- Turning HTML into editable design layers.
- Generating production code from artifacts.
- Webhooks and notifications (decision 19).
- Access for people without an account (decision 15).
- Runtime features for artifacts, such as shared data, file uploads or an artifact asking an AI model a
  question, the way some Claude artifacts can.
- **Later, perhaps:** branch previews, where an agent's unmerged branch shows as a version of its own.

---

## 12. How it was built and checked

Artifex was built in three phases.

- **Phase 1 — projects over MCP.** The Go module, configuration, logging, and the SQLite store with its
  migrations. Accounts, Google sign-in, the development sign-in, Admin verification and tokens. Project
  repositories (decisions 6–8), project roles (decision 16) and path safety. The MCP server with the project,
  file, publish, history, diff, restore, URL and member tools. Signed content addresses with isolation and
  CSP (decisions 10 and 11), and the bridge's size and link messages. A first web app: the project list, and
  the artifact and canvas views.
- **Phase 2 — comments and history for people.** Threads and pins (decision 14): the bridge's comment mode,
  threads in the web app, and the thread tools over MCP. Following renames (§5.3, decision 7b). History,
  side-by-side comparison and the Restore button (§6). Pages for members and tokens, and Admin account
  management.
- **Phase 3 — the rest.** Live updates (decision 21). Git over HTTPS (decisions 6b and 9). The container
  image, the compose file, `artifex admin backup` and `artifex healthcheck`.
- **After phase 3.** The Tabler look (decision 22), with a confirmation dialog in place of the browser's,
  and gzipped static files. Artifacts may no longer navigate the page on their own (decision 10).

**What is checked:**

- unit and integration tests in every package (`go test ./...`, also under `-race`), including concurrent
  publishes, conflicts, moves, path safety, roles, invitations, restore, threads following moves, the MCP
  server through the SDK's own client, git clone and push with the real git client, every page of the web
  app, and the static files' compression and caching;
- an end-to-end run in headless Chromium against a local server: sign-in, an agent's publish, a comment
  placed by clicking an element, the agent reading it, changing the artifact and resolving the thread, the
  open page reloading by itself, links between artifacts, the canvas, history and compare, the app being
  unable to reach into an artifact, and an artifact being unable to navigate the page without a click.

A security review found no way for an artifact to read the session, act as the viewer or reach another
project. It found eight smaller problems, all fixed and covered by tests:

- paths starting with `"` or `:` were read by git as quoted or as pathspec magic, which let a publish
  overwrite a file without its base (now `GIT_LITERAL_PATHSPECS` and NUL-separated index input);
- following threads cost git runs per thread and commit (now cached per commit);
- the bridge's "open" message could send the page anywhere on the app (now only project files, and only
  after a click in the artifact);
- the event stream outlived membership; the sandbox allowed modals and downloads; the app's script rule
  also allowed published scripts; pushes had no object checks or size cap, and large files were read whole
  into memory (now streamed); the Google domain check trusted the address (now the `hd` claim).

---

## 13. Where things are in the code

| Package | What it holds |
|---|---|
| `cmd/artifex` | The binary: `serve`, `admin` (accounts, tokens, backup), `healthcheck` |
| `internal/config` | The YAML configuration and its defaults |
| `internal/gitrepo` | Every git operation: commits with compare-and-swap, reads at a commit, history, diffs, following moves |
| `internal/store` | SQLite: accounts, sessions, tokens, projects, members, invitations, threads |
| `internal/service` | The rules: roles, paths, publishing and conflicts, restore, members, threads, accounts, tokens |
| `internal/content` | Signed content addresses, the sandbox CSP, the bridge injection |
| `internal/mcpserver` | The MCP tools and the agents' instructions |
| `internal/githttp` | Git over HTTPS through `git http-backend` |
| `internal/events` | Live updates to open pages |
| `internal/web` | Sign-in, the pages, their JSON API, `static/app.js` (the pages) and `static/bridge.js` (inside artifacts). `static/app.css` holds the slate theme and Artifex's own styles; `static/vendor/tabler/` and `icons/` are Tabler and its icons, copied unchanged (decision 22) |
