#!/bin/sh
# onegw one-command installer.
#
#   curl -fsSL https://raw.githubusercontent.com/FreePeak/onegw/master/scripts/install.sh | sh
#
# What it does: downloads the latest release binary for your OS/arch, verifies
# it against the release's SHA256SUMS, installs it, writes a starter config
# (loopback bind) with a generated admin password, and starts the gateway on
# 127.0.0.1:8080. Credentials are minted ONCE at first install and NEVER
# rotated afterwards: reinstalls, updates, and restarts reuse the existing
# config's admin password and gateway keys verbatim (a re-run beside a
# running gateway only swaps the binary; it never touches the config).
#
# If the release for your platform is missing, build from source instead:
#   git clone https://github.com/FreePeak/onegw && cd onegw
#   go build -o onegw ./cmd/onegw   (then set ONEGW_BIN=/path/to/onegw, or copy
#   it into PATH and run with ONEGW_START=0)
#
# Env overrides:
#   ONEGW_BIN              install this local binary instead of downloading
#   ONEGW_VERSION          release tag (default: latest)
#   ONEGW_INSTALL_PREFIX   default /usr/local (falls back to ~/.local/bin)
#   ONEGW_HOME             config/log/pid dir (default ~/.onegw)
#   ONEGW_LISTEN           default 127.0.0.1:8080
#   ONEGW_KEYS             gateway client keys (default: generated)
#   ONEGW_START=0          install + config only, do not start
#   ONEGW_SERVICE=1        install as a persistent service (launchd on macOS,
#                          systemd user service on Linux) instead of nohup
set -eu
REPO=FreePeak/onegw
VERSION=${ONEGW_VERSION:-latest}
PREFIX=${ONEGW_INSTALL_PREFIX:-/usr/local}
CONFHOME=${ONEGW_HOME:-$HOME/.onegw}
LISTEN=${ONEGW_LISTEN:-127.0.0.1:8080}
HOSTPORT=${LISTEN#*:}

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
  Linux)  os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS: $(uname -s)" ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64)  arch=amd64 ;;
  *) die "unsupported arch: $(uname -m)" ;;
esac

# --- fetch -------------------------------------------------------------------
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if [ -n "${ONEGW_BIN:-}" ]; then
  cp "$ONEGW_BIN" "$tmp/onegw"
  say "using local binary $ONEGW_BIN"
else
  if [ "$VERSION" = latest ]; then
    VERSION=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" | sed 's|.*/tag/||')
    [ -n "$VERSION" ] || die "could not resolve latest release"
  fi
  say "downloading onegw $VERSION ($os/$arch)..."
  if ! curl -fL --retry 3 -o "$tmp/onegw" \
      "https://github.com/$REPO/releases/download/$VERSION/onegw-$os-$arch"; then
    die "no release asset for $os/$arch at $VERSION; build from source instead:
  git clone https://github.com/$REPO && cd onegw && go build -o onegw ./cmd/onegw
  (see README 'Build from source'; then re-run with ONEGW_BIN=/path/to/onegw)"
  fi
  # Verify against the release's SHA256SUMS when it exists (published by the
  # release workflow). Missing file or missing tool: verify what we can.
  want=""
  got=""
  if curl -fL --retry 3 -o "$tmp/SHA256SUMS" \
      "https://github.com/$REPO/releases/download/$VERSION/SHA256SUMS" 2>/dev/null; then
    want=$(grep " onegw-$os-$arch\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)
    if [ -n "${want:-}" ]; then
      if command -v sha256sum >/dev/null 2>&1; then
        got=$(sha256sum "$tmp/onegw" | cut -d' ' -f1)
      elif command -v shasum >/dev/null 2>&1; then
        got=$(shasum -a 256 "$tmp/onegw" | cut -d' ' -f1)
      fi
      if [ -z "$got" ]; then
        say "note: no sha256 tool found; skipping checksum verification"
      elif [ "$got" != "$want" ]; then
        die "checksum mismatch for onegw-$os-$arch (got sha256:$got, want sha256:$want)"
      else
        say "checksum verified (sha256:$got)"
      fi
    else
      say "note: SHA256SUMS has no entry for onegw-$os-$arch; skipping verification"
    fi
  fi
fi
chmod 0755 "$tmp/onegw"

# --- install -----------------------------------------------------------------
BINDIR="$PREFIX/bin"
if ! mkdir -p "$BINDIR" 2>/dev/null || [ ! -w "$BINDIR" ]; then
  BINDIR="$HOME/.local/bin"
  mkdir -p "$BINDIR"
  say "note: $PREFIX not writable, installing to $BINDIR (add it to PATH)"
fi
install -m 0755 "$tmp/onegw" "$BINDIR/onegw"

# Verify the installed binary actually runs and reports the expected version
# (the release binaries are version-stamped by the release workflow).
if ! out=$("$BINDIR/onegw" version 2>/dev/null) || [ -z "$out" ]; then
  die "installed binary failed to run 'onegw version'; see README 'Build from source'"
fi
say "installed: $out"

# --- gateway keys ------------------------------------------------------------
# Credential-preservation contract: whatever keys the EXISTING config (or a
# previously installed service file) carries are the keys clients already
# use — a re-run must never rotate them. Resolution order:
#   1. existing $CONFHOME/onegw.toml [auth] keys (flat or [[auth.keys]]
#      tables — the first key of the first form found)
#   2. previously installed service file (launchd plist / systemd unit)
#   3. explicit ONEGW_KEYS env (only when the config has NO keys at all:
#      env replaces config keys entirely, so it must not shadow them)
#   4. generate fresh (first install only)
KEYS=""
if [ -f "$CONFHOME/onegw.toml" ]; then
  # Flat form: keys = ["sk-...", "sk-..."] — first entry.
  KEYS=$(sed -n 's/^[[:space:]]*keys[[:space:]]*=[[:space:]]*\[\{0,1\}"\([^",]*\)"\{0,1\}.*/\1/p' "$CONFHOME/onegw.toml" | head -1)
  if [ -z "$KEYS" ]; then
    # Table form: [[auth.keys]] with key = "sk-..." inside — first entry.
    KEYS=$(sed -n 's/^[[:space:]]*key[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFHOME/onegw.toml" | head -1)
  fi
fi
if [ -z "$KEYS" ]; then
  _PLIST="$HOME/Library/LaunchAgents/com.freepeak.onegw.plist"
  _UNIT="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/onegw.service"
  if [ -f "$_PLIST" ]; then
    KEYS=$(sed -n 's|.*<key>ONEGW_KEYS</key><string>\(.*\)</string>.*|\1|p' "$_PLIST" | head -1)
  elif [ -f "$_UNIT" ]; then
    KEYS=$(sed -n 's/^Environment=ONEGW_KEYS=//p' "$_UNIT" | head -1)
  fi
fi
if [ -z "$KEYS" ]; then
  KEYS=${ONEGW_KEYS:-}
fi
if [ -z "$KEYS" ]; then
  if command -v openssl >/dev/null 2>&1; then
    KEYS=$(openssl rand -hex 16)
  else
    KEYS=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
  fi
fi
KEYS_TOML="\"$(printf '%s' "$KEYS" | sed 's/,/", "/g')\""

# A live gateway is the owner of its credentials: whatever password and
# keys its config holds are what clients already use. A re-run must never
# start a second instance beside it (SO_REUSEPORT lets a newcomer share
# the port — traffic would silently split across two configs), and never
# mint fresh credentials a running config does not know about.
if [ -f "$CONFHOME/onegw.pid" ] && kill -0 "$(cat "$CONFHOME/onegw.pid")" 2>/dev/null; then
  say "onegw already running (pid $(cat "$CONFHOME/onegw.pid")); restart it to pick up the new binary"
  say "endpoint:   http://127.0.0.1:$HOSTPORT (config: $CONFHOME/onegw.toml)"
  exit 0
fi
if curl -fsS -o /dev/null -m 2 "http://127.0.0.1:$HOSTPORT/" 2>/dev/null; then
  say "binary updated; something else is already serving http://127.0.0.1:$HOSTPORT"
  say "(no pid file at $CONFHOME/onegw.pid). NOT starting a second gateway on a"
  say "shared port: under SO_REUSEPORT it would silently split traffic with the"
  say "running instance and its config owns the live credentials. Restart that"
  say "instance to pick up the new binary, or set ONEGW_LISTEN for a separate one."
  exit 0
fi
# --- starter config ----------------------------------------------------------
mkdir -p "$CONFHOME/data"
if [ ! -f "$CONFHOME/onegw.toml" ]; then
  if command -v openssl >/dev/null 2>&1; then
    ADMIN_PW=$(openssl rand -hex 12)
  else
    ADMIN_PW=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
  fi
  {
    echo "# generated by install.sh $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "[server]"
    echo "listen = \"$LISTEN\""
    echo "data_dir = \"$CONFHOME/data\""
    echo "admin_password = \"$ADMIN_PW\""
    echo ""
    echo "[auth]"
    echo "keys = [$KEYS_TOML]"
    echo ""
    echo "[saver]"
    echo "enabled = true"
    echo ""
    echo "# Add [[providers]] blocks here — see:"
    echo "# https://github.com/$REPO#configuration"
  } > "$CONFHOME/onegw.toml"
  say "wrote starter config $CONFHOME/onegw.toml (admin password: $ADMIN_PW)"
fi

# --- start -------------------------------------------------------------------
if [ "${ONEGW_START:-1}" = "0" ]; then
  say "installed $BINDIR/onegw (ONEGW_START=0, not starting)"
  exit 0
fi


# Service files pin ONEGW_KEYS in the environment ONLY when the config has
# no [auth] keys at all (older installs — flat form OR [[auth.keys]] tables;
# env replaces config keys entirely per config.go, so a pinned env would
# make later config-key edits silently ignored). When any keys exist in the
# config, the config wins and the service file pins nothing.
PLIST_KEYS=""
UNIT_KEYS=""
CFG_HAS_KEYS=""
if [ -f "$CONFHOME/onegw.toml" ]; then
  if grep -q '^[[:space:]]*keys[[:space:]]*=' "$CONFHOME/onegw.toml" \
     || grep -q '^[[:space:]]*key[[:space:]]*=[[:space:]]*"' "$CONFHOME/onegw.toml"; then
    CFG_HAS_KEYS=1
  fi
fi
if [ -z "$CFG_HAS_KEYS" ]; then
  PLIST_KEYS="    <key>ONEGW_KEYS</key><string>$KEYS</string>"
  UNIT_KEYS="Environment=ONEGW_KEYS=$KEYS"
fi
if [ "${ONEGW_SERVICE:-0}" = "1" ]; then
  say "installing persistent service (ONEGW_SERVICE=1)..."
  case "$(uname -s)" in
    Darwin)
      PLIST_DIR="$HOME/Library/LaunchAgents"
      PLIST="$PLIST_DIR/com.freepeak.onegw.plist"
      mkdir -p "$PLIST_DIR"
      cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.freepeak.onegw</string>
  <key>ProgramArguments</key>
  <array>
    <string>$BINDIR/onegw</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>ONEGW_CONFIG</key><string>$CONFHOME/onegw.toml</string>
$PLIST_KEYS
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$CONFHOME/onegw.log</string>
  <key>StandardErrorPath</key><string>$CONFHOME/onegw.log</string>
</dict>
</plist>
EOF
      launchctl bootout "gui/$UID/com.freepeak.onegw" 2>/dev/null || true
      launchctl bootstrap "gui/$UID" "$PLIST" || die "launchctl bootstrap failed; check: launchctl print gui/$UID/com.freepeak.onegw"
      say "service: launchd (com.freepeak.onegw) — $(launchctl print "gui/$UID/com.freepeak.onegw" >/dev/null 2>&1 && echo loaded || echo FAILED)"
      ;;
    Linux)
      UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
      UNIT="$UNIT_DIR/onegw.service"
      mkdir -p "$UNIT_DIR"
      cat > "$UNIT" <<EOF
[Unit]
Description=onegw LLM gateway
After=network-online.target

[Service]
ExecStart=$BINDIR/onegw
Environment=ONEGW_CONFIG=$CONFHOME/onegw.toml
$UNIT_KEYS
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
EOF
      systemctl --user daemon-reload
      systemctl --user enable --now onegw.service
      if systemctl --user is-active --quiet onegw.service; then
        say "service: systemd user unit onegw.service (active)"
      else
        say "WARN: unit installed but not active; check: journalctl --user -u onegw"
      fi
      say "  stop:        systemctl --user stop onegw.service"
      ;;
  esac
else
  if [ -n "$UNIT_KEYS" ]; then
    nohup env ONEGW_CONFIG="$CONFHOME/onegw.toml" ONEGW_KEYS="$KEYS" \
      "$BINDIR/onegw" >> "$CONFHOME/onegw.log" 2>&1 &
  else
    nohup env ONEGW_CONFIG="$CONFHOME/onegw.toml" \
      "$BINDIR/onegw" >> "$CONFHOME/onegw.log" 2>&1 &
  fi
  echo $! > "$CONFHOME/onegw.pid"
fi
# --- verify ------------------------------------------------------------------
# Probe the unauthenticated dashboard root — /admin/* is password-gated.
ok=""
i=0
while [ $i -lt 20 ]; do
  if curl -fsS -o /dev/null "http://127.0.0.1:$HOSTPORT/" 2>/dev/null; then
    ok=1
    break
  fi
  i=$((i + 1))
  sleep 1
done
[ -n "$ok" ] || die "gateway did not become healthy; check $CONFHOME/onegw.log"

say ""
say "onegw is running:"
say "  endpoint:    http://127.0.0.1:$HOSTPORT"
say "  dashboard:   http://127.0.0.1:$HOSTPORT/  (password: $(sed -n 's/^admin_password[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFHOME/onegw.toml" | head -1))"
say "  gateway key: $KEYS  (clients send this as their API key; sourced from the existing config when one exists)"
say "  config:      $CONFHOME/onegw.toml"
say "  log:         $CONFHOME/onegw.log"
if [ -f "$CONFHOME/onegw.pid" ]; then
  say "  pid:         $(cat "$CONFHOME/onegw.pid")"
  say "  stop:        kill \$(cat $CONFHOME/onegw.pid)"
else
  say "  managed by launchd/systemd; use launchctl/systemctl to stop"
fi
