#!/bin/sh
# devmon-agent installer.
#
# Takes a clean Linux host from nothing to a paired device. It resolves the two
# host prerequisites that otherwise look like agent bugs — the docker socket
# GID and the ownership of the state directory — writes a compose file, starts
# the agent, and prints the CA fingerprint and the first pairing code.
#
# POSIX sh on purpose: `sh` and `docker` are the only prerequisites, and both
# are already required to run the agent at all. No bashisms, no `pipefail`
# (which is not POSIX), no arrays.
#
# SPDX-License-Identifier: AGPL-3.0-only

set -eu

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

# IMAGE_REPO and IMAGE_TAG name the released image the compose file pulls.
# They must stay in step with README.md and compose.example.yaml, which name
# the same tag.
IMAGE_REPO='ghcr.io/scnplt/devmon-agent'
IMAGE_TAG='0.2.0'

# NONROOT_UID is the UID the distroless/static:nonroot image runs as. The state
# directory must be owned by it or startup fails at MkdirAll with "permission
# denied", which reads as an agent fault rather than a host one.
NONROOT_UID='65532'
NONROOT_GID='65532'

# SOCKET_PATH is where the installer looks for the Docker socket to read its
# group ownership from. It is not configurable: an agent talking to a remote
# Docker host is outside what this installer sets up.
SOCKET_PATH='/var/run/docker.sock'

# CONTAINER_BINARY is the absolute path of the agent binary inside the image.
# The image is distroless/static:nonroot — there is no shell in it — so every
# in-container command must name the binary directly. `docker compose exec sh
# -c ...` does not work and never will.
CONTAINER_BINARY='/usr/local/bin/devmon-agent'

# SERVICE_NAME is the compose service the exec and log commands address.
SERVICE_NAME='devmon-agent'

# READY_TIMEOUT_SECONDS bounds how long the installer waits for the agent to
# answer /v1/status before giving up and telling the operator where to look.
# Generous, because the first start generates a CA and a server keypair.
READY_TIMEOUT_SECONDS='60'
READY_POLL_SECONDS='2'

# MAX_PORT is the highest TCP port a value may name.
MAX_PORT='65535'

# Defaults, each matching internal/config/config.go so the installer never
# writes a value that differs from what the agent would have chosen anyway.
DEFAULT_POLICY_MODE='default'
DEFAULT_PORT='8443'
DEFAULT_STATE_DIR='/var/lib/devmon'
DEFAULT_LOG_MAX_AGE_DAYS='1'
DEFAULT_LOG_MAX_TOTAL_MB='64'
DEFAULT_AUDIT_MAX_AGE_DAYS='365'
DEFAULT_AUDIT_MAX_ROWS='100000'
DEFAULT_DEVICE_NAME='my-phone'

# STATUS_PAYLOAD is filled by wait_ready and read by print_fingerprint.
STATUS_PAYLOAD=''

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------

info() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

# run prints a command before executing it, so an operator can see exactly what
# was done to their host — and, under --dry-run, executes nothing at all.
run() {
	printf '    $ %s\n' "$*"
	if [ "$DRY_RUN" = 'yes' ]; then
		return 0
	fi
	"$@"
}

# ---------------------------------------------------------------------------
# Options
# ---------------------------------------------------------------------------

DRY_RUN='no'
FORCE='no'
ASSUME_YES='no'

# Every prompt is overridable by a flag and by an environment variable of the
# same name, so the script is usable unattended. The environment supplies the
# initial value; a flag overrides it; an empty value falls through to a prompt.
PUBLIC_ADDR="${DEVMON_PUBLIC_ADDR:-}"
POLICY_MODE="${DEVMON_POLICY_MODE:-}"
PORT="${DEVMON_PORT:-}"
STATE_DIR="${DEVMON_STATE_DIR:-}"
LOG_MAX_AGE_DAYS="${DEVMON_LOG_MAX_AGE_DAYS:-}"
LOG_MAX_TOTAL_MB="${DEVMON_LOG_MAX_TOTAL_MB:-}"
AUDIT_MAX_AGE_DAYS="${DEVMON_AUDIT_MAX_AGE_DAYS:-}"
AUDIT_MAX_ROWS="${DEVMON_AUDIT_MAX_ROWS:-}"
INSTALL_DIR="${DEVMON_INSTALL_DIR:-}"
DEVICE_NAME="${DEVMON_DEVICE_NAME:-}"

usage() {
	cat <<'EOF'
devmon-agent installer

Usage: ./install.sh [options]

Options (each also settable by the environment variable in brackets):

  --public-addr ADDR      Hostname or IP the phone will reach this host at.
                          Required. Comma-separated for several. No scheme and
                          no port — "vps.example.com", not "https://vps:8443".
                          [DEVMON_PUBLIC_ADDR]
  --policy-mode MODE      read-only | default | full. Fixed at startup and
                          immutable thereafter; a client can never widen it.
                          Default: default. [DEVMON_POLICY_MODE]
  --port PORT             Host port to publish. Default: 8443. [DEVMON_PORT]
  --state-dir DIR         Host directory holding the agent's identity and
                          audit record. Default: /var/lib/devmon.
                          [DEVMON_STATE_DIR]
  --log-max-age-days N    Operational log retention in days. Default: 1.
                          [DEVMON_LOG_MAX_AGE_DAYS]
  --log-max-total-mb N    Operational log budget in MB. Default: 64.
                          [DEVMON_LOG_MAX_TOTAL_MB]
  --audit-max-age-days N  Audit retention in days. Must be at least the
                          operational log retention — the security record has
                          to outlive debug output. Default: 365.
                          [DEVMON_AUDIT_MAX_AGE_DAYS]
  --audit-max-rows N      Audit row ceiling. Default: 100000.
                          [DEVMON_AUDIT_MAX_ROWS]
  --install-dir DIR       Where compose.yaml is written. Default: the current
                          directory. [DEVMON_INSTALL_DIR]
  --device-name NAME      Name for the first paired device. Default: my-phone.
                          [DEVMON_DEVICE_NAME]

  --dry-run               Print the compose file and every command, execute
                          nothing, and touch no file on this host.
  --force                 Overwrite an existing compose.yaml in the install
                          directory. Never overwrites the state directory.
  --yes, -y               Accept every default without prompting. Requires
                          --public-addr (or DEVMON_PUBLIC_ADDR).
  -h, --help              Show this help.

Upgrading an existing installation is not this script's job:

  docker compose pull && docker compose up -d
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--public-addr)
		PUBLIC_ADDR="${2:-}"
		shift 2
		;;
	--policy-mode)
		POLICY_MODE="${2:-}"
		shift 2
		;;
	--port)
		PORT="${2:-}"
		shift 2
		;;
	--state-dir)
		STATE_DIR="${2:-}"
		shift 2
		;;
	--log-max-age-days)
		LOG_MAX_AGE_DAYS="${2:-}"
		shift 2
		;;
	--log-max-total-mb)
		LOG_MAX_TOTAL_MB="${2:-}"
		shift 2
		;;
	--audit-max-age-days)
		AUDIT_MAX_AGE_DAYS="${2:-}"
		shift 2
		;;
	--audit-max-rows)
		AUDIT_MAX_ROWS="${2:-}"
		shift 2
		;;
	--install-dir)
		INSTALL_DIR="${2:-}"
		shift 2
		;;
	--device-name)
		DEVICE_NAME="${2:-}"
		shift 2
		;;
	--dry-run)
		DRY_RUN='yes'
		shift
		;;
	--force)
		FORCE='yes'
		shift
		;;
	--yes | -y)
		ASSUME_YES='yes'
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		die "unknown option: $1 (try --help)"
		;;
	esac
done

# ---------------------------------------------------------------------------
# Validators
#
# Each returns 0 for an acceptable value. They exist so a bad value is caught
# at prompt time, with the reason, rather than making the agent exit 2 twenty
# seconds later with the operator already looking somewhere else.
# ---------------------------------------------------------------------------

# is_safe_name gates the device name, which reaches a `docker compose exec`
# argument. It is passed as a single quoted word, so this is defence in depth
# rather than the only thing standing between the value and a shell.
is_safe_name() {
	[ -n "$1" ] || return 1
	! has_unsafe_chars "$1"
}

is_positive_int() {
	case "$1" in
	'' | *[!0-9]*) return 1 ;;
	esac
	[ "$1" -ge 1 ]
}

is_valid_port() {
	is_positive_int "$1" || return 1
	[ "$1" -le "$MAX_PORT" ]
}

# has_unsafe_chars rejects any character outside an allowlist of what a
# hostname, an IP, or a filesystem path can legitimately contain. It is an
# allowlist rather than a blocklist because every value this script accepts is
# interpolated into the generated compose file and into docker invocations.
#
# The type-specific validators below already reject what an operator would
# plausibly mistype. What this catches is a value arriving from an environment
# variable during an automated install: a `"` would otherwise close the YAML
# scalar and let the caller append compose keys of their own — `privileged:
# true`, or a bind mount of `/` — to a file this script then hands to `docker
# compose up -d`. The agent already holds the Docker socket, so that is a host
# compromise rather than a malformed config file.
has_unsafe_chars() {
	[ -n "$(printf '%s' "$1" | tr -d 'A-Za-z0-9._:/-')" ]
}

is_absolute_path() {
	case "$1" in
	/*) ;;
	*) return 1 ;;
	esac
	! has_unsafe_chars "$1"
}

is_valid_policy_mode() {
	case "$1" in
	read-only | default | full) return 0 ;;
	esac
	return 1
}

# is_valid_san mirrors internal/config/config.go's isValidSAN: an entry
# carrying a port or a path is rejected outright, because "host:8443" is the
# most likely operator mistake here and accepting it would produce a
# certificate that matches nothing.
is_valid_san() {
	case "$1" in
	'' | *:* | */*) return 1 ;;
	esac
	! has_unsafe_chars "$1"
}

# is_valid_public_addr accepts a comma-separated list where every entry passes
# is_valid_san, matching how the agent parses DEVMON_PUBLIC_ADDR.
is_valid_public_addr() {
	[ -n "$1" ] || return 1

	rest="$1"
	while [ -n "$rest" ]; do
		case "$rest" in
		*,*)
			entry="${rest%%,*}"
			rest="${rest#*,}"
			;;
		*)
			entry="$rest"
			rest=''
			;;
		esac
		is_valid_san "$entry" || return 1
	done
	return 0
}

# ---------------------------------------------------------------------------
# Prompts
# ---------------------------------------------------------------------------

# prompt reads one value into the variable named by $1, reprompting until the
# validator named in $4 accepts the answer. A value already supplied by a flag
# or the environment is validated but never re-asked; under --yes, or with no
# terminal on stdin, the default is taken instead of prompting.
prompt() {
	var_name="$1"
	question="$2"
	default_value="$3"
	validator="$4"

	eval "current=\${$var_name}"

	if [ -n "$current" ]; then
		if ! "$validator" "$current"; then
			die "$var_name is set to '$current', which is not accepted. See --help for the rule."
		fi
		return 0
	fi

	if [ "$ASSUME_YES" = 'yes' ] || [ ! -t 0 ]; then
		[ -n "$default_value" ] ||
			die "'$question' has no default; pass it as a flag or an environment variable for unattended use."
		eval "$var_name=\$default_value"
		return 0
	fi

	while :; do
		if [ -n "$default_value" ]; then
			printf '%s [%s]: ' "$question" "$default_value"
		else
			printf '%s: ' "$question"
		fi
		read -r answer || die 'no more input; pass every value as a flag for unattended use.'
		[ -n "$answer" ] || answer="$default_value"
		if "$validator" "$answer"; then
			eval "$var_name=\$answer"
			return 0
		fi
		info '  that value is not accepted; see --help for the rule.'
	done
}

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------

COMPOSE_CMD=''

preflight() {
	step 'Checking prerequisites'

	command -v docker >/dev/null 2>&1 ||
		die 'docker is not on PATH. Install it first: https://docs.docker.com/engine/install/'
	info '  docker: found'

	docker info >/dev/null 2>&1 ||
		die 'the Docker daemon did not answer "docker info". Start it, or add this user to the docker group, and try again.'
	info '  docker daemon: responding'

	if docker compose version >/dev/null 2>&1; then
		COMPOSE_CMD='docker compose'
	elif command -v docker-compose >/dev/null 2>&1; then
		COMPOSE_CMD='docker-compose'
	else
		die 'neither "docker compose" nor "docker-compose" is available. Install the Compose plugin: https://docs.docker.com/compose/install/'
	fi
	info "  compose: $COMPOSE_CMD"
}

# resolve_socket_gid reads the group that owns the Docker socket. group_add
# must equal it, or the agent's startup ping fails with "permission denied" on
# the socket. 999 is right on Debian and Ubuntu and wrong elsewhere, so this is
# resolved rather than assumed — and the script fails loudly rather than
# guessing when neither stat dialect works.
resolve_socket_gid() {
	[ -S "$SOCKET_PATH" ] ||
		die "$SOCKET_PATH is not a socket. This installer sets up an agent that talks to the local Docker daemon over that path."

	if gid="$(stat -c '%g' "$SOCKET_PATH" 2>/dev/null)" && [ -n "$gid" ]; then
		printf '%s' "$gid"
		return 0
	fi
	# BSD stat (macOS, some minimal images) spells the same thing differently.
	if gid="$(stat -f '%g' "$SOCKET_PATH" 2>/dev/null)" && [ -n "$gid" ]; then
		printf '%s' "$gid"
		return 0
	fi
	die "could not read the group of $SOCKET_PATH with either \`stat -c\` or \`stat -f\`. Find it by hand and set group_add in compose.yaml yourself."
}

# as_root runs a command with sudo only when not already root, and prints it
# either way. Nothing privileged happens on this host without being shown.
as_root() {
	if [ "$(id -u)" = '0' ]; then
		run "$@"
	else
		command -v sudo >/dev/null 2>&1 ||
			die "this step needs root and sudo is not available. Re-run as root: $*"
		run sudo "$@"
	fi
}

# ---------------------------------------------------------------------------
# Steps
# ---------------------------------------------------------------------------

check_state_dir() {
	step "Checking the state directory ($STATE_DIR)"

	if [ ! -d "$STATE_DIR" ]; then
		info '  does not exist yet; it will be created'
		return 0
	fi

	# An existing, non-empty state directory holds the CA, the device
	# registry, and the audit record. Touching it would unpair every device
	# and destroy the security history, so the installer refuses outright
	# rather than offering to merge or overwrite.
	if [ -n "$(ls -A "$STATE_DIR" 2>/dev/null)" ]; then
		die "$STATE_DIR already exists and is not empty.

This looks like an existing installation. Its contents are the agent's
identity: overwriting them would unpair every device and destroy the audit
record, so this installer will not touch it.

To upgrade an existing installation instead:

    cd <the directory holding your compose.yaml>
    $COMPOSE_CMD pull && $COMPOSE_CMD up -d

To install alongside it, pass --state-dir with a different path."
	fi

	info '  exists and is empty; it will be reused'
}

prepare_state_dir() {
	step "Preparing the state directory ($STATE_DIR)"
	info "  the image runs as UID $NONROOT_UID, so the directory must be owned by it"
	as_root mkdir -p "$STATE_DIR"
	as_root chown "$NONROOT_UID:$NONROOT_GID" "$STATE_DIR"
	as_root chmod 700 "$STATE_DIR"
}

compose_file_contents() {
	cat <<EOF
# Written by devmon-agent's install.sh. Safe to edit and re-apply with
# \`$COMPOSE_CMD up -d\`.

services:
  $SERVICE_NAME:
    image: $IMAGE_REPO:$IMAGE_TAG

    # Pinned so the agent can name itself through DEVMON_SELF_CONTAINER below.
    # Without it compose derives the container name from the project directory,
    # which changes if this file moves.
    container_name: $SERVICE_NAME
    restart: unless-stopped

    ports:
      - "$PORT:8443"

    volumes:
      # A BIND MOUNT, deliberately — not a named volume. The operator can see,
      # back up, and restore it, and \`down -v\` cannot destroy it. This
      # directory holds the agent's identity: losing it unpairs every device.
      - $STATE_DIR:/var/lib/devmon

      # :ro does not prevent writes through the Docker API — the API is
      # request/response over the socket — but it does prevent the socket file
      # itself being replaced, and it states intent.
      - $SOCKET_PATH:/var/run/docker.sock:ro

    # Resolved from this host at install time with \`stat\`, not assumed. 999
    # is right on Debian and Ubuntu and wrong elsewhere.
    group_add:
      - "$SOCKET_GID"

    environment:
      DEVMON_PUBLIC_ADDR: "$PUBLIC_ADDR"
      DEVMON_POLICY_MODE: "$POLICY_MODE"

      # How the agent recognises its own container, so it can refuse to stop
      # or delete itself. It can usually work this out unaided, but the name
      # pinned above is the one form that stays true across a recreate — an ID
      # would be stale the next time this file changes.
      DEVMON_SELF_CONTAINER: "$SERVICE_NAME"

      DEVMON_LOG_MAX_AGE_DAYS: "$LOG_MAX_AGE_DAYS"
      DEVMON_LOG_MAX_TOTAL_MB: "$LOG_MAX_TOTAL_MB"
      DEVMON_AUDIT_MAX_AGE_DAYS: "$AUDIT_MAX_AGE_DAYS"
      DEVMON_AUDIT_MAX_ROWS: "$AUDIT_MAX_ROWS"

      # Rate limits. Raise them if a busy host or a chatty client trips them;
      # there is deliberately no value that turns a limiter off.
      # DEVMON_RATE_STATUS_PER_MIN: "30"
      # DEVMON_RATE_PAIR_PER_MIN: "5"
      # DEVMON_RATE_GUARDED_PER_SEC: "20"
EOF
}

write_compose_file() {
	step "Writing $COMPOSE_PATH"

	if [ -e "$COMPOSE_PATH" ] && [ "$FORCE" != 'yes' ]; then
		die "$COMPOSE_PATH already exists. Re-run with --force to overwrite it, or pass --install-dir to write somewhere else."
	fi

	if [ "$DRY_RUN" = 'yes' ]; then
		info '  --dry-run: the file below would be written, and was not'
		info ''
		compose_file_contents
		return 0
	fi

	mkdir -p "$INSTALL_DIR"
	compose_file_contents >"$COMPOSE_PATH"
	info '  written'
}

start_agent() {
	step 'Starting the agent'
	# shellcheck disable=SC2086
	# COMPOSE_CMD is either "docker compose" (two words) or "docker-compose",
	# so it must split into arguments. Its value is set by preflight, never by
	# an operator.
	run $COMPOSE_CMD -f "$COMPOSE_PATH" up -d
}

# status_payload fetches /v1/status over TLS without verification. That is
# correct here and only here: the CA does not exist until the agent has
# started, so there is nothing yet to verify against, and the request never
# leaves the loopback interface. The fingerprint printed from this payload is
# read on the host the operator already trusts, which is the entire reason it
# is not fetched from anywhere else.
status_payload() {
	curl -sk --max-time 5 "https://127.0.0.1:$PORT/v1/status" 2>/dev/null
}

wait_ready() {
	step 'Waiting for the agent to answer /v1/status'

	if [ "$DRY_RUN" = 'yes' ]; then
		info '  --dry-run: skipped'
		return 0
	fi

	command -v curl >/dev/null 2>&1 ||
		die "curl is required to confirm the agent started. Install curl, or check by hand: curl -sk https://127.0.0.1:$PORT/v1/status"

	waited=0
	while [ "$waited" -lt "$READY_TIMEOUT_SECONDS" ]; do
		if payload="$(status_payload)" && [ -n "$payload" ]; then
			STATUS_PAYLOAD="$payload"
			info "  ready after ${waited}s"
			return 0
		fi
		sleep "$READY_POLL_SECONDS"
		waited=$((waited + READY_POLL_SECONDS))
	done

	die "the agent did not answer /v1/status within ${READY_TIMEOUT_SECONDS}s. Look at its output with:

    $COMPOSE_CMD -f $COMPOSE_PATH logs $SERVICE_NAME"
}

# json_field pulls one string field out of the status payload. The payload is
# a flat object of known keys — its field allowlist is a security boundary, so
# it does not nest — which makes a jq dependency unnecessary.
json_field() {
	printf '%s' "$1" | sed -n 's/.*"'"$2"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
}

print_fingerprint() {
	step 'CA fingerprint'

	if [ "$DRY_RUN" = 'yes' ]; then
		info '  --dry-run: skipped'
		return 0
	fi

	fingerprint="$(json_field "$STATUS_PAYLOAD" 'ca_fingerprint')"
	if [ -z "$fingerprint" ]; then
		warn 'could not read ca_fingerprint from /v1/status; read it by hand before pairing.'
		return 0
	fi

	info ''
	info "    $fingerprint"
	info ''
	info '  Write this down somewhere off this host, now. It is what proves the'
	info '  agent your phone talks to later is this agent, and not something'
	info '  standing in front of it. Record it from this terminal — never from'
	info '  the network you are protecting against.'
}

print_pairing_code() {
	step 'First pairing code'

	if [ "$DRY_RUN" = 'yes' ]; then
		info '  --dry-run: skipped'
		return 0
	fi

	info "  minting a single-use code for device \"$DEVICE_NAME\""
	info ''

	# The code goes to this terminal and nowhere else: never to a file, never
	# through `tee`, never into anything this script also logs. It is a
	# credential, and a copy that outlives the terminal is a copy that can be
	# stolen. Same rule as runDevicePairCode in cmd/devmon-agent/cli.go.
	#
	# The image is distroless/static:nonroot — no shell — so the binary is
	# named by its absolute path rather than run through `sh -c`.
	# shellcheck disable=SC2086
	$COMPOSE_CMD -f "$COMPOSE_PATH" exec -T "$SERVICE_NAME" \
		"$CONTAINER_BINARY" device pair-code --name "$DEVICE_NAME" ||
		die "could not mint a pairing code. The agent is running; mint one by hand with:

    $COMPOSE_CMD -f $COMPOSE_PATH exec -T $SERVICE_NAME $CONTAINER_BINARY device pair-code --name $DEVICE_NAME"
}

print_next_steps() {
	if [ "$DRY_RUN" = 'yes' ]; then
		step 'Dry run complete'
		cat <<EOF

  Nothing on this host was created, changed, or started. The compose file
  above is what a real run would write to $COMPOSE_PATH.

  Re-run without --dry-run to install.

EOF
		return 0
	fi

	step 'Done'
	cat <<EOF

  The agent is running and listening on port $PORT.

  Next:
    1. Open the DevMon app and pair with the code above, checking the
       fingerprint shown before it. The code is single-use and expires.
    2. Do not expose this port to the open internet without a VPN or a
       firewall in front of it. docs/THREAT-MODEL.md says what the agent does
       and does not defend against.
    3. Back up $STATE_DIR. It is the agent's identity, and the backup is
       itself a credential. See docs/BACKUP.md.

  Useful commands:
    $COMPOSE_CMD -f $COMPOSE_PATH logs -f $SERVICE_NAME
    $COMPOSE_CMD -f $COMPOSE_PATH exec -T $SERVICE_NAME $CONTAINER_BINARY device list
    $COMPOSE_CMD -f $COMPOSE_PATH pull && $COMPOSE_CMD -f $COMPOSE_PATH up -d

EOF
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

info 'devmon-agent installer'
if [ "$DRY_RUN" = 'yes' ]; then
	info '(--dry-run: nothing on this host will be created, changed, or started)'
fi

preflight

step 'Configuration'
prompt PUBLIC_ADDR \
	'Hostname or IP the phone will reach this host at (no scheme, no port)' \
	'' is_valid_public_addr
prompt POLICY_MODE 'Policy mode (read-only | default | full)' "$DEFAULT_POLICY_MODE" is_valid_policy_mode
prompt PORT 'Host port to publish' "$DEFAULT_PORT" is_valid_port
prompt STATE_DIR 'State directory' "$DEFAULT_STATE_DIR" is_absolute_path
prompt LOG_MAX_AGE_DAYS 'Operational log retention (days)' "$DEFAULT_LOG_MAX_AGE_DAYS" is_positive_int
prompt LOG_MAX_TOTAL_MB 'Operational log budget (MB)' "$DEFAULT_LOG_MAX_TOTAL_MB" is_positive_int
prompt AUDIT_MAX_AGE_DAYS 'Audit retention (days)' "$DEFAULT_AUDIT_MAX_AGE_DAYS" is_positive_int
prompt AUDIT_MAX_ROWS 'Audit row ceiling' "$DEFAULT_AUDIT_MAX_ROWS" is_positive_int
prompt INSTALL_DIR 'Directory to write compose.yaml into' "$PWD" is_absolute_path
prompt DEVICE_NAME 'Name for the first paired device' "$DEFAULT_DEVICE_NAME" is_safe_name

# The agent enforces this too, and rejects the pair at startup with exit 2.
# Catching it here means the operator finds out before anything is written.
if [ "$AUDIT_MAX_AGE_DAYS" -lt "$LOG_MAX_AGE_DAYS" ]; then
	die "audit retention ($AUDIT_MAX_AGE_DAYS days) is shorter than operational log retention ($LOG_MAX_AGE_DAYS days). The security record must outlive debug output."
fi

COMPOSE_PATH="$INSTALL_DIR/compose.yaml"

SOCKET_GID="$(resolve_socket_gid)"
info "  docker socket GID: $SOCKET_GID (resolved from $SOCKET_PATH)"

check_state_dir
prepare_state_dir
write_compose_file
start_agent
wait_ready
print_fingerprint
print_pairing_code
print_next_steps
