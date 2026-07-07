#!/usr/bin/env python3
"""Claude Code Notification hook for the agent-deck conductor: push permission
prompts to Telegram so a gated command never wedges silently.

Reads hook JSON on stdin ({"message": ..., "hook_event_name": "Notification"})
and sends it to the Telegram user configured in
~/.config/agent-deck/config.toml [conductor.telegram].

Always exits 0 — a push failure must never block the conductor.
"""
import json
import os
import sys
import tomllib
import urllib.request

LOG = os.path.expanduser("~/.local/share/agent-deck/conductor/main/push-hook.log")


def log(msg):
    try:
        from datetime import datetime, timezone
        with open(LOG, "a") as f:
            f.write(f"{datetime.now(timezone.utc).isoformat()} notify: {msg}\n")
    except Exception:
        pass


def main():
    data = json.load(sys.stdin)
    message = data.get("message") or "(no detail provided)"

    with open(os.path.expanduser("~/.config/agent-deck/config.toml"), "rb") as f:
        tg = tomllib.load(f)["conductor"]["telegram"]
    token, chat_id = tg["token"], tg["user_id"]
    if not token or not chat_id:
        log("skip: telegram not configured")
        return

    text = ("⚠️ conductor-main is waiting on an approval prompt:\n\n"
            f"{message[:3500]}\n\n"
            "It stays blocked until answered. Attach to the session to approve "
            "(or ask your local Claude to look).")
    body = json.dumps({"chat_id": chat_id, "text": text}).encode()
    req = urllib.request.Request(
        f"https://api.telegram.org/bot{token}/sendMessage",
        data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        ok = json.load(resp).get("ok")
    log(f"pushed permission prompt: ok={ok}")


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        log(f"error: {type(e).__name__}: {e}")
    sys.exit(0)
