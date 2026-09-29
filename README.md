# claude-mattermost-bridge

Mattermost bot that drives claude-app-server-go (~/claude-app-server-go)
over its JSON-RPC WebSocket. Modelled on `codex-msngr-bridge`, reduced to the core.
Go, one dependency (`github.com/coder/websocket`), single static binary.

## How it works

- The bot logs in with a Mattermost bot token: REST for posting, WebSocket for
  incoming `posted` events. Both reconnect with backoff.
- **DM**: one Claude thread per DM channel, replies are flat.
- **Channels / group DMs** (`MM_ALLOW_CHANNELS=1`): an `@bot` mention opens a
  Claude thread bound to that Mattermost thread; replies go into it. Follow-ups
  in that thread need no mention. Prompts are prefixed with `@author:`.
- Only usernames in `MM_ALLOWED_USERS` are served; everyone else is ignored.
- Every finished text item of a turn is posted as its own message (interim
  commentary and the final answer). Thinking and tool output are not forwarded
  (`BRIDGE_SHOW_TOOLS=1` adds one line per tool call). A typing indicator shows
  while the agent works. Long output is split under Mattermost's post limit.
- A message sent while a turn runs goes through `turn/steer`, i.e. it is queued
  as the next turn.
- A conversation idle for `BRIDGE_IDLE_CLOSE` (default 1h) has its server thread
  closed to free the slot and process; the next message re-attaches to the same
  session, so nothing is lost.
- Conversation-to-session mappings are kept in a JSON state file, so contexts
  survive restarts (see Limitations); `!new` forgets the saved session.
- Attached files are downloaded to `BRIDGE_ATTACHMENT_DIR` (mode 600) and passed
  to Claude as local paths; the app server must share the filesystem.

## Files and images from Claude

The bridge tells Claude (via the server's `append_system_prompt`, on every start
and resume of a thread) to reference files it wants delivered as a standalone
line, outside code blocks:

```markdown
![short caption](/absolute/path/plot.png)
[report.csv](/absolute/path/report.csv)
```

Such lines are removed from the text, the files are uploaded to Mattermost and
attached to that message. Details:

- Images and other files both become Mattermost attachments (images preview
  inline). The link label of a plain file is its file name; an image keeps its
  own name, and its caption is shown only when the reply has no other text.
- Links inside sentences, web links and anything in code fences are left alone.
  Relative paths resolve against `CLAUDE_CWD`; `sandbox:/abs/path` works too.
- **Allowlist.** The agent's output is untrusted (a prompt injection could ask it
  to send `~/.ssh/id_rsa`), so files must lie under `CLAUDE_CWD`, the attachment
  directory, `/tmp`, `/var/tmp` or a directory in `BRIDGE_SEND_ROOTS`. Symlinks
  are resolved first, so a link cannot lead outside. Anything else is refused
  with a visible notice in the reply.
- Limits: 8 files per message, `BRIDGE_MAX_FILE_MB` (default 50) each, and
  Mattermost's own upload limit; 10 attachments fit on one post, more are sent
  in follow-up posts. The same file twice is sent once.
- The app server and the bridge must share the filesystem (same paths).
- `BRIDGE_SEND_FILES=0` turns the feature off (no instructions, no uploads).

## Commands

Mattermost swallows `/...` as its own slash commands, so the bridge uses `!`.

| Command | Effect |
|---|---|
| `!help` | list commands |
| `!status` | thread id, permission mode, running turns |
| `!new` | fresh context (also `!clear`); closes the old thread on the server |
| `!cancel` | interrupt running work and queued messages |
| `!model [name]` | list the server's models, or switch this conversation's model (partial names work; `default` resets) |
| `!mode [m]` | show / change permission mode via `approval/respond` |

The model list comes from the Claude CLI through the server's `model/list`, so it
is always current. A selection is per conversation, is saved in the state file,
and applies immediately to a running thread (`thread/set_model`). `CLAUDE_MODEL`
sets the initial model for new conversations.

Unknown `!words` are sent to Claude as ordinary text.

## Setup

1. Mattermost: System Console > Integrations > Bot Accounts > create a bot, copy
   its access token into a mode-600 file. Add the bot to the channels you want.
2. Start `claude-app-server start` and note its `ws://...?key=...` URL.
3. Build and configure:

   ```sh
   CGO_ENABLED=0 go build -ldflags="-s -w" -o claude-mattermost-bridge ./cmd/claude-mattermost-bridge
   cp bot.env.example bot.env   # edit
   set -a; . ./bot.env; set +a
   ./claude-mattermost-bridge -check   # verifies the token only
   ./claude-mattermost-bridge
   ```

`deploy/claude-mattermost-bridge.service` is a user-level systemd unit.

## Limitations

- **Resume needs a recent app server.** Conversations survive restarts of the
  bridge and of claude-app-server: the bridge saves each conversation's Claude
  CLI session id in `BRIDGE_STATE_FILE` (after its first completed turn) and
  re-attaches with the server's `thread/attach`. Servers without that method
  fall back to a fresh context. A turn that was running during the restart is
  aborted. `CLAUDE_CWD` must not change, since the CLI stores sessions per
  directory; if a saved session cannot be resumed the bridge starts fresh and
  asks the user to resend.
- **No live permission prompts.** The server has no `can_use_tool` routing yet;
  denied tools are reported (`turn/permission_denied`) and the user can raise the
  mode with `!mode`. Default mode is `acceptEdits`.
- Posts made while the Mattermost WebSocket is down are not replayed.
- Posts edited or deleted after sending are ignored.

## License

MIT, see [LICENSE](LICENSE).
