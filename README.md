# claude-mattermost-bridge

Mattermost bot that drives claude-app-server-go (~/claude-app-server-go)
over its JSON-RPC WebSocket. Modelled on `codex-msngr-bridge`, reduced to the core.
Go, one dependency (`github.com/coder/websocket`), single static binary.

## How it works

- The bot logs in with a Mattermost bot token: REST for posting, WebSocket for
  incoming `posted` events. Both reconnect with backoff.
- **DM**: one Claude thread per DM channel, replies are flat. Only usernames in
  `MM_ALLOWED_USERS` may use the bot in DMs.
- **Channels / group DMs**: only those listed in `MM_ALLOWED_CHANNELS` (channel
  IDs or channel names, comma separated; empty means none). Inside a listed
  channel **every human is equal**: they can chat with the bot, answer its
  permission prompts and change its settings (`!mode`, `!trust`, `!model`,
  `!new`...). `MM_ALLOWED_USERS` does not apply there, and other bots are
  ignored. An `@bot` mention opens a Claude thread bound to that Mattermost
  thread; replies go into it, and follow-ups in it need no mention. Prompts are
  prefixed with `@author:` because several people can share a thread.
  Names are the URL name of the channel (not the display name) and are not
  unique across teams; use the channel ID (channel menu > View Info) when the
  bot is in several teams. `CLAUDE_CHANNEL_PERMISSION_MODE` sets a different
  permission mode for channel conversations, for example `default` so that
  edits ask first.
- Every finished text item of a turn is posted as its own message (interim
  commentary and the final answer). Thinking and tool output are not forwarded
  (`BRIDGE_SHOW_TOOLS=1` adds one line per tool call). A typing indicator shows
  while the agent works. Long output is split under Mattermost's post limit.
- Each agent message ends with an italic usage line, as of when it was written:
  `_opus 5.5 | medium | ctx 45.2k/1M | tokens 1.2M in, 38.0k out_`: model,
  effort, context in use / context window, and the tokens this conversation has
  read (cached input included, so it grows fast) and written. Token totals are
  saved with the conversation and reset by `!new`. Needs an app server with
  `thread/usage` and `thread/settings`; parts it does not report are left out.
- A message sent while a turn runs goes through `turn/steer`: it joins the
  running turn and the agent reads it at its next step.
- When the agent actually reads a message, the bot reacts to it: :eyes: for a
  message that started a turn, :writing_hand: for one steered into a running
  turn. Commands handled by the bridge itself get no reaction.
- A conversation idle for `BRIDGE_IDLE_CLOSE` (default 1h) has its server thread
  closed to free the slot and process; the next message re-attaches to the same
  session, so nothing is lost.
- Conversation-to-session mappings are kept in a JSON state file, so contexts
  survive restarts (see Limitations); `!new` forgets the saved session.
- Attached files are downloaded to `BRIDGE_ATTACHMENT_DIR` (mode 600) and passed
  to Claude as local paths; the app server must share the filesystem.

## Permission prompts

When Claude wants to run a tool that its permission mode does not pre-approve
(for example a shell command in `default` or `acceptEdits` mode), the bridge asks
in the chat instead of silently refusing. The prompt shows what is about to
happen (the command, the file and a preview of its content, or an edit's old and
new text) and the bot adds three reactions, so each answer is one click:

| Reaction | Effect |
|---|---|
| :white_check_mark: | allow this call |
| :x: | deny; Claude is told and reacts accordingly |
| :fast_forward: | allow, and trust this tool for the rest of the conversation: later calls run without asking (see below) |

Or reply `!allow`, `!allow always` (same as :fast_forward:) or `!deny [reason]`,
which answer the longest-waiting prompt. Once decided, the prompt collapses to one
line ("Allowed by @x: `Bash` `ls -la`") so answered prompts do not clutter the chat.
A prompt nobody answers is denied automatically after the app server's
`--permission-timeout` (default 5 minutes), and the post says so. `!cancel`
withdraws pending prompts.

**Too many prompts?** Three ways to cut them, from narrow to broad:

1. **Trust a tool.** :fast_forward: or `!trust Bash` makes this conversation run that
   tool without asking. `Write`, `Edit`, `MultiEdit` and `NotebookEdit` are one family
   (`!trust edit`). `!trust all` trusts everything, `!trust` shows the list and
   `!trust off [tool]` revokes. The list is saved with the conversation, so it
   survives restarts and `!new`. Trusted calls leave no post at all; set
   `BRIDGE_SHOW_TOOLS=1` if you want a one-line note per tool call.
2. **Auto mode.** `!mode auto` (or `CLAUDE_PERMISSION_MODE=auto`) lets the CLI decide:
   it approves what it judges safe and only asks about the rest. In testing it ran a
   multi-command task, including `rm -rf` inside the working directory and a
   download, with no prompts, so it is fairly permissive. Only some models support it.
3. **Prompts off.** `BRIDGE_PERMISSION_PROMPTS=0` restores the old behaviour: denied
   tools are reported afterwards and `!mode` raises the mode.

`CLAUDE_PERMISSION_MODE` decides what is asked at all: `default` asks for edits and
commands, `acceptEdits` only for commands, `auto` rarely, `bypassPermissions` never.

- In DMs only usernames in `MM_ALLOWED_USERS` can answer; in an allowed channel
  anyone can, and so can change the trust list, which then applies to everyone in
  that thread. Reactions from other bots and the bot's own are ignored.
- `!trust all` and `!trust Bash` are bridge-side and need no server flag, unlike
  `bypassPermissions`. They are as strong as clicking :white_check_mark: every time.
- Why reactions and not buttons: Mattermost buttons make the *Mattermost server*
  call a URL, so the bridge would need to be reachable from it, and Mattermost
  blocks calls to private addresses by default. Reactions arrive on the WebSocket
  the bridge already has open.
- Needs an app server with `permission/respond` (opt-in via `permission_prompts`);
  `auto` needs one that accepts that mode.

## Files and images from Claude

The bridge tells Claude (via the server's `append_system_prompt`, on every start
and resume of a thread) to reference files it wants delivered as a standalone
line, outside code blocks:

```markdown
![short caption](/absolute/path/plot.png)
[report.csv](/absolute/path/report.csv)
```

Links to absolute paths are also picked up inside a line of prose
(`[a.f90](/tmp/a.f90): the kernel`) and left in the text as `a.f90`; links in code
spans and fences, web links and relative links inside prose are left alone.

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
| `!reset` | reconnect the agent when a turn is stuck; keeps the conversation and its context (unlike `!new`) |
| `!context` | show what fills the context window (runs the CLI's `/context`) |
| `!compact [instructions]` | compact the context now (runs `/compact`); automatic compactions are reported too |
| `!cost` | token usage and cost of this conversation (runs `/cost`) |
| `!model [name]` | list the server's models, or switch this conversation's model (partial names work; `default` resets) |
| `!allow [always]`, `!deny [reason]` | answer the oldest permission prompt; `always` trusts the tool in this conversation |
| `!trust [tool...\|all\|off]` | show or change the tools this conversation runs without asking |
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

## Running as systemd user services

`deploy/` has units for both processes. The app server keeps its auth key in a
mode-600 file (`--key-file`, created on first start, reused afterwards) and the
bridge reads the same file (`APP_SERVER_KEY_FILE`), so restarts of either side
need no reconfiguration and the key never appears in logs or unit files.

```sh
# binaries
(cd ~/claude-app-server-go && CGO_ENABLED=0 go build -o ~/.local/bin/claude-app-server ./cmd/claude-app-server)
CGO_ENABLED=0 go build -o ~/.local/bin/claude-mattermost-bridge ./cmd/claude-mattermost-bridge

# config: absolute paths only (EnvironmentFile does not expand ~ or $HOME)
mkdir -p ~/.config/claude-mattermost ~/.config/systemd/user
install -m 600 bot.env.example ~/.config/claude-mattermost/bot.env   # then edit it
cp deploy/*.service ~/.config/systemd/user/

systemctl --user daemon-reload
systemctl --user enable --now claude-app-server claude-mattermost-bridge
loginctl enable-linger $USER      # keep running after logout
journalctl --user -u claude-mattermost-bridge -f
```

The services get only a minimal environment (`HOME`, `PATH`), so `claude` must
be logged in through its own config rather than shell variables. Restart the
app server after upgrading it; the bridge reconnects by itself and resumes
conversations. After changing `bot.env`, restart the bridge.

## Limitations

- **Resume needs a recent app server.** Conversations survive restarts of the
  bridge and of claude-app-server: the bridge saves each conversation's Claude
  CLI session id in `BRIDGE_STATE_FILE` (after its first completed turn) and
  re-attaches with the server's `thread/attach`. Servers without that method
  fall back to a fresh context. A turn that was running during the restart is
  aborted. `CLAUDE_CWD` must not change, since the CLI stores sessions per
  directory; if a saved session cannot be resumed the bridge starts fresh and
  asks the user to resend.
- Posts made while the Mattermost WebSocket is down are not replayed.
- Posts edited or deleted after sending are ignored.

## License

MIT, see [LICENSE](LICENSE).
