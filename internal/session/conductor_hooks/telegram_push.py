#!/usr/bin/env python3
"""Claude Code Stop hook for the agent-deck conductor: push turn replies to
Telegram via sendMessage (outbound HTTPS only).

Reads hook JSON on stdin, extracts completed turn replies from the transcript,
and sends any not-yet-pushed ones to the Telegram user configured in
~/.config/agent-deck/config.toml [conductor.telegram].

State file tracks the last-handled entry so replies missed by a raced/merged
Stop event (e.g. a user message queued during a heartbeat turn) are swept up
by the next invocation instead of silently dropped.

Skips turns triggered by [HEARTBEAT] nudges so the 15-min heartbeat doesn't
spam the phone (NEED: escalations still reach Telegram via the bridge's own
heartbeat parsing).

Always exits 0 — a push failure must never block the conductor.
"""
import json
import os
import sys
import tomllib
import urllib.request

BASE = os.path.expanduser("~/.local/share/agent-deck/conductor/main")
LOG = f"{BASE}/push-hook.log"
STATE = f"{BASE}/push-hook-state.json"
TELEGRAM_LIMIT = 3900  # hard API limit is 4096; leave headroom for prefix/marker
MAX_PUSHES_PER_RUN = 3


def log(msg):
    try:
        from datetime import datetime, timezone
        with open(LOG, "a") as f:
            f.write(f"{datetime.now(timezone.utc).isoformat()} {msg}\n")
    except Exception:
        pass


def completed_turns(transcript_path):
    """Return ([(uuid, assistant_text, triggering_user_text)], newest_reply_ts).

    A turn's reply is the final assistant text entry before the next real user
    message (tool_result user entries are machine turns, not boundaries).
    """
    turns = []
    newest_ts = ""
    cur_text = cur_uuid = None
    cur_trigger = pending_trigger = None
    with open(transcript_path) as f:
        for line in f:
            try:
                e = json.loads(line)
            except json.JSONDecodeError:
                continue
            if e.get("isSidechain"):
                continue
            etype = e.get("type")
            msg = e.get("message") or {}
            content = msg.get("content")
            if etype == "user":
                if isinstance(content, list) and any(
                        isinstance(b, dict) and b.get("type") == "tool_result"
                        for b in content):
                    continue
                if isinstance(content, str):
                    user_text = content
                elif isinstance(content, list):
                    user_text = "\n".join(b.get("text", "") for b in content
                                          if isinstance(b, dict) and b.get("type") == "text")
                else:
                    continue
                if not user_text:
                    continue
                # real user message = boundary: previous turn is complete
                if cur_text is not None:
                    turns.append((cur_uuid, cur_text, cur_trigger))
                    cur_text = cur_uuid = None
                pending_trigger = user_text
            elif etype == "assistant":
                texts = [b.get("text", "") for b in (content or [])
                         if isinstance(b, dict) and b.get("type") == "text"]
                if texts:
                    cur_text = "\n".join(texts)
                    cur_uuid = e.get("uuid")
                    cur_trigger = pending_trigger
                    ts = e.get("timestamp") or ""
                    if ts > newest_ts:
                        newest_ts = ts
    if cur_text is not None:
        turns.append((cur_uuid, cur_text, cur_trigger))
    return turns, newest_ts


def send_telegram(token, chat_id, text):
    if len(text) > TELEGRAM_LIMIT:
        text = text[:TELEGRAM_LIMIT] + "\n… (truncated)"
    body = json.dumps({"chat_id": chat_id, "text": "🔔 " + text}).encode()
    req = urllib.request.Request(
        f"https://api.telegram.org/bot{token}/sendMessage",
        data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.load(resp).get("ok")


def main():
    data = json.load(sys.stdin)
    transcript = data.get("transcript_path")
    if not transcript or not os.path.exists(transcript):
        log(f"skip: no transcript ({transcript})")
        return

    # A Stop event means a reply just completed, but its transcript entry can
    # lag the event by ~0.1-1s. Re-read until we see a freshly-written reply.
    import time
    from datetime import datetime, timezone, timedelta
    turns = []
    for attempt in range(10):
        turns, newest_ts = completed_turns(transcript)
        fresh_cutoff = (datetime.now(timezone.utc) - timedelta(seconds=15)
                        ).strftime("%Y-%m-%dT%H:%M:%S.%fZ")
        if newest_ts >= fresh_cutoff:
            break
        time.sleep(0.5)
    if not turns:
        log("skip: no completed turns in transcript")
        return

    last_uuid = None
    try:
        with open(STATE) as f:
            last_uuid = json.load(f).get("last_uuid")
    except Exception:
        pass

    idx = next((i for i, (u, _, _) in enumerate(turns) if u == last_uuid), None)
    # unknown state (first run / rotated transcript): handle only the newest turn
    pending = turns[idx + 1:] if idx is not None else turns[-1:]
    pending = pending[-MAX_PUSHES_PER_RUN:]

    with open(os.path.expanduser("~/.config/agent-deck/config.toml"), "rb") as f:
        tg = tomllib.load(f)["conductor"]["telegram"]
    token, chat_id = tg["token"], tg["user_id"]
    if not token or not chat_id:
        log("skip: telegram not configured")
        return

    for uuid, text, trigger in pending:
        if trigger and "[HEARTBEAT]" in trigger:
            log(f"skip: heartbeat-triggered turn ({uuid})")
        else:
            ok = send_telegram(token, chat_id, text)
            log(f"pushed: ok={ok} chars={len(text)} ({uuid})")

    with open(STATE, "w") as f:
        json.dump({"last_uuid": turns[-1][0]}, f)


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        log(f"error: {type(e).__name__}: {e}")
    sys.exit(0)
