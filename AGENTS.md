---
description: onegw gateway restart and process-safety rules — never kill a running onegw without warning, restart with the correct config/data-dir, KindCursor is a no-account case
paths: ["**/AGENTS.md", "**/RULES.md", "**/onegw-agent.md", "onegw/.cursor/rules/*"]
---

# onegw agent rules

## 1. Never kill a running onegw without warning

onegw runs as a shared gateway serving many agents and sessions.
Before stopping/restarting it, tell the user exactly what will happen
and ask for approval. Killing the process interrupts every client
connected through it.

Exception: if the running binary is provably stale (e.g. a fix was
committed but not rebuilt into the running process) and the user has
already said "restart it", restart without asking.

## 2. Restarting onegw after a rebuild

The running gateway is started with `-config onegw.toml` from the repo
root and uses `data_dir = "/Users/linh.doan/.onegw/data"` from that
config. After rebuilding the binary (`go build -o ~/.local/bin/onegw
./cmd/onegw`), restart with:

```bash
kill <pid>; ONEGW_DATA_DIR=/Users/linh.doan/.onegw/data ~/.local/bin/onegw -bg -config onegw.toml
```

Do NOT start a second instance — onegw refuses to bind if a second
one is already running (SO_REUSEPORT splits traffic and breaks
rate windows). Check `ps aux | grep onegw` first.

## 3. Cursor model discovery is a no-account case

`KindCursor` provider defs in onegw.toml have **zero** `[[providers.accounts]]`.
Cursor's AgentService/ChatService Connect-RPC endpoints (ListModels,
GetModels, GetCatalog) all return 404, so FetchModels short-circuits
in `fetchProviderModels` (`internal/server/admin_models.go`) and returns
a curated catalog. A 200 with a non-empty body from a 0-account def is
a success — do NOT surface "no models in the response" for Cursor.

## 4. Validate port/process state before restart

Check what is actually listening before restart: `lsof -i :8080 -sTCP:LISTEN`
or `ps aux | grep onegw`. Confirm PID matches the running binary you
expect before killing it.

## 5. Never rebuild or restart the live onegw binary

The operator manages the build and deploy lifecycle themselves (updating
the version in onegw settings, triggering the rebuild). Agents must
**never** run `go build`, `kill`, or any restart command against the
live onegw process (`~/.local/bin/onegw` or the PID from `ps aux | grep onegw`).
Config changes in `onegw.toml` are picked up on the operator's next
reload — do not attempt to force a reload.

## 6. Audit for leaked secrets before every commit or push

Never commit, push, or paste a credential — API keys, OAuth/JWT tokens,
Codebuff `authToken`, cookies, session ids. The runtime `onegw.toml` is
gitignored; that is the first line of defense, not the only one.

Before `git commit` and again before `git push`, audit what is actually
about to leave the machine. Read the real value in-process and print only
YES/NO, so the audit output cannot leak the secret it is looking for:

```bash
python3 - <<'PY'
import json, os, re, subprocess
cfg = open(os.path.expanduser("~/.onegw/onegw.toml")).read()
needles = []
d = json.load(open(os.path.expanduser("~/.config/manicode/credentials.json")))["default"]
needles += [d["authToken"]]
needles += [m for m in re.findall(r'api_key = "([^"]+)"', cfg)]
run = lambda *a: subprocess.run(a, capture_output=True, text=True).stdout
objs = [l.split()[0] for l in run("git", "rev-list", "--objects", "HEAD").splitlines() if l.strip()]
hits = sum(1 for o in objs if any(n in run("git", "cat-file", "-p", o) for n in needles if n))
print("blobs containing a live credential:", hits)   # must be 0
PY
```

Also check the commit patches and the PR payload (title, body, comments)
— a secret pasted into a PR body outlives the commit history. When a real
credential was ever exposed, say so plainly and recommend rotation at the
issuer (signing out of the provider web session invalidates browser-session
JWTs) instead of quietly rewriting history.

