#!/bin/sh
# install-ffmpeg-streamer-service.sh - install the ffmpeg-streamer systemd unit.
#
# The unit loops list.txt (ffmpeg concat demuxer) into the local mediamtx RTSP
# server, running with the checkout directory as its working directory. The
# unit has Requires=/After= on the mediamtx unit, so it only runs when
# mediamtx does.
#
# Usage:
#   scripts/install-ffmpeg-streamer-service.sh
#
# Optional environment overrides:
#   FFMPEG_BIN    absolute path to the ffmpeg binary (default: resolved via PATH)
#   SERVICE_USER  user the service runs as (default: the invoking user)
#   UNIT_NAME     systemd unit name (default: ffmpeg-streamer.service)
#   MEDIAMTX_UNIT mediamtx unit this stream depends on (default: mediamtx.service)
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)

UNIT_NAME=${UNIT_NAME:-ffmpeg-streamer.service}
UNIT_TEMPLATE=$SCRIPT_DIR/${UNIT_NAME%.service}.service
UNIT_DST=/etc/systemd/system/$UNIT_NAME
SERVICE_USER=${SERVICE_USER:-${SUDO_USER:-$(id -un)}}
MEDIAMTX_UNIT=${MEDIAMTX_UNIT:-mediamtx.service}

fail() { echo "error: $*" >&2; exit 1; }

command -v systemctl >/dev/null 2>&1 || fail "systemctl not found; this script requires systemd."
[ -f "$UNIT_TEMPLATE" ] || fail "unit template not found: $UNIT_TEMPLATE"
[ -f "$PROJECT_DIR/list.txt" ] || fail "concat playlist not found: $PROJECT_DIR/list.txt (create it first; format: https://ffmpeg.org/ffmpeg-formats.html#concat)"

FFMPEG_BIN=${FFMPEG_BIN:-$(command -v ffmpeg 2>/dev/null || true)}
[ -n "$FFMPEG_BIN" ] || fail "ffmpeg not found in PATH; install it or set FFMPEG_BIN"
case $FFMPEG_BIN in
	/*) ;;
	*) fail "ffmpeg path is not absolute: $FFMPEG_BIN" ;;
esac
[ -x "$FFMPEG_BIN" ] || fail "ffmpeg binary missing or not executable: $FFMPEG_BIN"

# systemd splits ExecStart on whitespace and the unit file has no way to
# escape spaces in paths, so a checkout under such a path cannot work.
case $PROJECT_DIR in
	*[[:space:]]*) fail "project path contains whitespace: $PROJECT_DIR" ;;
esac
case $FFMPEG_BIN in
	*[[:space:]]*) fail "ffmpeg path contains whitespace: $FFMPEG_BIN" ;;
esac

SUDO=
if [ "$(id -u)" -ne 0 ]; then
	command -v sudo >/dev/null 2>&1 || fail "not running as root and sudo not found"
	SUDO=sudo
fi

echo "Installing $UNIT_NAME:"
echo "  project dir  : $PROJECT_DIR"
echo "  ffmpeg binary: $FFMPEG_BIN"
echo "  service user : $SERVICE_USER"
echo "  mediamtx unit: $MEDIAMTX_UNIT"
echo "  destination  : $UNIT_DST"

# Escape the sed replacement texts (& is special in replacements, | is the
# delimiter) so unusual-but-legal path characters survive substitution.
ESC_PROJECT_DIR=$(printf '%s' "$PROJECT_DIR" | sed 's/[&|\\]/\\&/g')
ESC_SERVICE_USER=$(printf '%s' "$SERVICE_USER" | sed 's/[&|\\]/\\&/g')
ESC_FFMPEG_BIN=$(printf '%s' "$FFMPEG_BIN" | sed 's/[&|\\]/\\&/g')
ESC_MEDIAMTX_UNIT=$(printf '%s' "$MEDIAMTX_UNIT" | sed 's/[&|\\]/\\&/g')

RENDERED=$(mktemp)
trap 'rm -f "$RENDERED"' EXIT
sed -e "s|@PROJECT_DIR@|$ESC_PROJECT_DIR|g" \
	-e "s|@SERVICE_USER@|$ESC_SERVICE_USER|g" \
	-e "s|@FFMPEG_BIN@|$ESC_FFMPEG_BIN|g" \
	-e "s|@MEDIAMTX_UNIT@|$ESC_MEDIAMTX_UNIT|g" \
	"$UNIT_TEMPLATE" >"$RENDERED"

$SUDO install -m 644 "$RENDERED" "$UNIT_DST"
$SUDO systemctl daemon-reload

SERVICE=${UNIT_NAME%.service}
echo
echo "Installed. Next steps:"
echo "  sudo systemctl enable --now $SERVICE   # start now and on boot (starts $MEDIAMTX_UNIT first)"
echo "  sudo systemctl status $SERVICE"
echo "  journalctl -u $SERVICE -f              # follow logs"
