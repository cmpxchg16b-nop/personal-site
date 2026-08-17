#!/bin/sh
# install-systemd-service.sh - install the personal-site systemd unit.
#
# The unit runs the ./server binary from this checkout with the checkout
# directory as its working directory. --config-xml is relative to the working
# directory, and the providers re-read the document on every request, so
# editing serverConfig.xml later needs no service restart.
#
# Usage:
#   scripts/install-systemd-service.sh
#
# Optional environment overrides:
#   SERVICE_USER   user the service runs as (default: the invoking user)
#   UNIT_NAME      systemd unit name (default: personal-site.service)
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)

UNIT_NAME=${UNIT_NAME:-personal-site.service}
UNIT_TEMPLATE=$SCRIPT_DIR/${UNIT_NAME%.service}.service
UNIT_DST=/etc/systemd/system/$UNIT_NAME
SERVICE_USER=${SERVICE_USER:-${SUDO_USER:-$(id -un)}}

fail() { echo "error: $*" >&2; exit 1; }

command -v systemctl >/dev/null 2>&1 || fail "systemctl not found; this script requires systemd."
[ -f "$UNIT_TEMPLATE" ] || fail "unit template not found: $UNIT_TEMPLATE"
[ -x "$PROJECT_DIR/server" ] || fail "server binary missing or not executable: $PROJECT_DIR/server (build it first: cd web/site && npm ci && npm run build && cd ../.. && go build -o server ./cmd/server)"
[ -f "$PROJECT_DIR/serverConfig.xml" ] || fail "config not found: $PROJECT_DIR/serverConfig.xml"

# systemd splits ExecStart on whitespace and the unit file has no way to
# escape spaces in paths, so a checkout under such a path cannot work.
case $PROJECT_DIR in
	*[[:space:]]*) fail "project path contains whitespace: $PROJECT_DIR" ;;
esac

SUDO=
if [ "$(id -u)" -ne 0 ]; then
	command -v sudo >/dev/null 2>&1 || fail "not running as root and sudo not found"
	SUDO=sudo
fi

echo "Installing $UNIT_NAME:"
echo "  project dir : $PROJECT_DIR"
echo "  service user: $SERVICE_USER"
echo "  destination : $UNIT_DST"

# Escape the sed replacement texts (& is special in replacements, | is the
# delimiter) so unusual-but-legal path characters survive substitution.
ESC_PROJECT_DIR=$(printf '%s' "$PROJECT_DIR" | sed 's/[&|\\]/\\&/g')
ESC_SERVICE_USER=$(printf '%s' "$SERVICE_USER" | sed 's/[&|\\]/\\&/g')

RENDERED=$(mktemp)
trap 'rm -f "$RENDERED"' EXIT
sed -e "s|@PROJECT_DIR@|$ESC_PROJECT_DIR|g" \
	-e "s|@SERVICE_USER@|$ESC_SERVICE_USER|g" \
	"$UNIT_TEMPLATE" >"$RENDERED"

$SUDO install -m 644 "$RENDERED" "$UNIT_DST"
$SUDO systemctl daemon-reload

SERVICE=${UNIT_NAME%.service}
echo
echo "Installed. Next steps:"
echo "  sudo systemctl enable --now $SERVICE   # start now and on boot"
echo "  sudo systemctl status $SERVICE"
echo "  journalctl -u $SERVICE -f              # follow logs"
