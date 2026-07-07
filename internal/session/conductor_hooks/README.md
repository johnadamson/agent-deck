# Conductor Telegram hooks

Claude Code hook scripts that make the conductor's remote-channel (Telegram)
experience reliable. They exist because the Python bridge's synchronous
"send → wait → capture last output" reply path races the conductor's
multi-turn work and can forward a stale/previous reply (upstream issues
around #876). These hooks make replies **event-driven** instead.

## Scripts

- **`telegram_push.py`** — a `Stop` hook. On every completed conductor turn it
  parses the transcript, extracts each turn's final assistant message, and
  pushes not-yet-sent ones to Telegram via `sendMessage` (outbound HTTPS only,
  firewall-friendly). Keeps `push-hook-state.json` (last-handled entry uuid) so
  replies missed by a merged/raced Stop event are swept on the next invocation.
  Retries the transcript read (~5s) because entries flush slightly after the
  Stop event fires. Skips `[HEARTBEAT]`-triggered turns.

- **`telegram_notify.py`** — a `Notification` hook (matcher
  `permission_prompt|elicitation_dialog`). Pushes any permission prompt to
  Telegram so a gated command never wedges silently in a terminal nobody is
  watching.

Both read Telegram creds from `~/.config/agent-deck/config.toml`
`[conductor.telegram]` (`token`, `user_id`) and always exit 0 — a push failure
must never block the conductor.

## Integration status

**Currently these are installed by hand** (copied to `~/.local/bin/`, wired into
the per-conductor `.claude/settings.json` `hooks` block). They are committed here
as the source of truth.

TODO to make `conductor setup` install them automatically (mirrors how
`conductor_bridge.py` is handled):

1. `go:embed` these scripts (see `conductor_bridge_embed.go` for the pattern) and
   write them into the conductor dir during setup.
2. Extend the settings generator (`conductor_claude_settings.go`) to emit a
   `hooks` block (`Stop` → telegram_push, `Notification` → telegram_notify)
   alongside the `permissions` it already writes.

## Related: dispatch permissions

For the conductor to dispatch autonomously over Telegram, `session send`/
`session start` need to be in the settings `allow` list rather than `ask`.
`conductor_claude_settings.go` deliberately keeps them in `ask` (see
`TestConductorClaudeSettings_LifecycleAndMutatingPrompt` and the maintainer's
security rationale). The intended upstream-friendly change is an opt-in config
knob (e.g. `[conductor] auto_allow_dispatch = true`) that moves them to `allow`
at generation time, leaving the default posture unchanged. Not yet implemented
here — currently applied by hand-editing the generated settings.
