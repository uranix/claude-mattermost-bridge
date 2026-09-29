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
- Attached files are downloaded to `BRIDGE_ATTACHMENT_DIR` (mode 600) and passed
  to Claude as local paths; the app server must share the filesystem.

## Commands

Mattermost swallows `/...` as its own slash commands, so the bridge uses `!`.

| Command | Effect |
|---|---|
| `!help` | list commands |
| `!status` | thread id, permission mode, running turns |
| `!new` | fresh context (also `!clear`) |
| `!cancel` | interrupt running work and queued messages |
| `!mode [m]` | show / change permission mode via `approval/respond` |

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

- **No persistence.** claude-app-server keeps threads in memory per connection.
  If it restarts or the connection drops, every conversation starts a fresh
  context (users get a notice if work was running). Fixing this properly needs a
  server method to attach an existing CLI session id.
- **No live permission prompts.** The server has no `can_use_tool` routing yet;
  denied tools are reported (`turn/permission_denied`) and the user can raise the
  mode with `!mode`. Default mode is `acceptEdits`.
- Posts made while the Mattermost WebSocket is down are not replayed.
- Outgoing files/images (Claude producing attachments) are not implemented;
  the codex bridge does this via Markdown links in the final answer.
- Posts edited or deleted after sending are ignored.
