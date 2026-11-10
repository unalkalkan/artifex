# Artifex — MCP tools

Agents use Artifex through MCP at `<base_url>/mcp` (Streamable HTTP, stateless, JSON answers), with an
Artifex token as a bearer token:

```sh
claude mcp add --transport http artifex https://<address>/mcp --header "Authorization: Bearer <token>"
```

A token acts as the person who made it, with that person's role in each project (design decision 16):
**Commenter** reads and comments, **Editor** also publishes and restores, **Owner** also manages members.
Errors come back as tool errors with a message meant for the agent.

The server also sends agents a short guide in its MCP instructions (`internal/mcpserver/mcpserver.go`,
`instructions`). Keep that guide and this page in step.

## Files and versions

### `list_projects`

The projects the account belongs to: `project`, `title`, `role`, `version` (the latest), `updated_at`, `url`.

### `create_project`

| Argument | |
|---|---|
| `project` | New id: 1–64 lowercase letters, digits and dashes |
| `title` | Optional; stored in `project.yaml` |

The caller becomes the Owner. `upload_artifact` also creates projects.

### `upload_artifact`

Writes one file as a new version, creating the project when it doesn't exist.

| Argument | |
|---|---|
| `project`, `path` | Where. `path` is relative to the project's root |
| `content` or `content_base64` | Text, or binary as base64 |
| `base` | The blob id of the version you changed. Required when the file exists |
| `message` | Shown in the history |
| `title` | The project's title, used only when this call creates the project |

Answer: `version`, `project_created`, `unchanged`, `files` (each with its new `blob`, the base for your next
change, and a `url` for HTML files), `warnings` (broken relative links and the like), `url`.

### `publish`

Writes, moves and deletes several files as one version.

| Argument | |
|---|---|
| `project`, `message` | |
| `changes` | A list of `{path, content \| content_base64, base, move_to, delete}` |

- A new file has no `base`. Changing, moving or deleting an existing one needs its `base`.
- If any of those files changed since its base, **nothing** is written: the error lists each file with its
  `current_blob` and `current_content`. Merge and publish again with `current_blob` as the base.
- `move_to` may come with new content; threads follow the moved artifact.
- The project must exist (`create_project`, or an earlier `upload_artifact`).
- One publish holds up to `limits.max_publish_bytes` of files (50 MB by default), each up to
  `limits.max_file_bytes` (25 MB). The request may be a third larger, for base64.

### `list_files`, `read_file`

`list_files(project, path?, version?, recursive?)` lists a folder with each file's `blob` and `size`.
`read_file(project, path, version?)` returns `content` (or `content_base64`) and `blob`.

### `history`, `diff`, `restore`

- `history(project, path?, limit?, skip?)`: versions, newest first; with `path`, the versions that changed
  that file, following renames.
- `diff(project, from?, to?, path?)`: the source diff; `to` defaults to the latest, `from` to `to`'s parent.
- `restore(project, version, path?, message?)`: brings back a file, a folder, or the whole project as it was
  at `version`, as a new version. Nothing in between is lost.

### `get_url`

`get_url(project, path?, version?)`: the address people open.

## Members (Owners)

- `list_members(project)`: members and invitations with their roles (any member).
- `set_member(project, email, role)`: `commenter`, `editor` or `owner`. An email with no account yet becomes
  an invitation, applied when that person's account is verified.
- `remove_member(project, email)`. A project always keeps at least one Owner.

## Comments

- `list_threads(project?, path?, state?, since?, limit?)`: `state` is `open` or `resolved`; `since` is an
  RFC 3339 time, which makes this the change feed. Each thread has its `path` (where the artifact is now),
  the `version` it was made on, its `anchor` and its `entries`.
- `get_thread(id)`: also `excerpt`, the artifact's HTML around the element, at the thread's version, and
  `changed_since`.
- `create_thread(project, path, body, version?, anchor?)`: an `anchor` is `{selector, text, x, y, ox, oy,
  width}`, every field optional.
- `reply(id, body)`, `resolve_thread(id, version?, body?)`, `reopen_thread(id, body?)`. Resolve with the
  version of your publish: people then get a before-and-after comparison.

## What an artifact may do

- It's an HTML file, drawn as written. `<title>` is its name.
- `<meta name="artifact" content="width=1440">` shows it at a fixed width; without it, it fills the window.
- It may load files only from its own project, by relative path: no CDNs. Scripts are allowed.
- Relative links to other artifacts open them inside Artifex.
- A folder can have `folder.yaml` (`title`, `label`, `order`), and the project `project.yaml` (`title`,
  `description`, `open`).
