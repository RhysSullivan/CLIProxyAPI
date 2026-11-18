#!/usr/bin/env bash
# Deploy vibeproxy-engine v8.0.77-rhys.1 over v8.0.64-rhys.2 on minisforum,
# with recovery that does not depend on the deploying shell.
#
# ---------------------------------------------------------------- how to run
#
#   bash deploy-v8.0.77-rhys.1.sh check-only   # preflight + baseline; changes nothing
#   bash deploy-v8.0.77-rhys.1.sh launch       # start the deploy in its own systemd unit
#   bash deploy-v8.0.77-rhys.1.sh status       # print the recorded state and the log path
#
# `launch` freezes a copy of this script into the state directory and runs it
# as a transient user unit (vibeproxy-deploy-<epoch>), so the agent that
# launched it can lose its connection when the proxy restarts without
# affecting the deploy. It returns at once and prints the log path.
#
# State, backups and logs: ~/.local/state/vibeproxy-deploy/v8.0.77-rhys.1/
# (outside ~/.cli-proxy-api, which the engine watches as its auth dir).
# Each deploy log ends with a RESULT line. The guard appends its own lines to
# the same log.
#
# ---------------------------------------------------------------- what it does
#
#  1. Lock (flock). A second run while one holds the lock refuses and changes
#     nothing. A run after a recorded success says so and changes nothing.
#  2. Preflight. Refuses unless the installed binary is exactly v8.0.64-rhys.2
#     (pinned sha256), the running process is that same file, the candidate is
#     exactly v8.0.77-rhys.1 (pinned sha256), and the config has no top-level
#     `claude:` or `upstream:` block yet.
#  3. Baseline on the running old binary: /healthz, /v1/models, one real Claude
#     and one real Codex request. A provider fault aborts here, before any change.
#  4. Backs up the binary and the config, writes the state file, then ARMS THE
#     GUARD: an enabled systemd user timer + service (vibeproxy-deploy-guard)
#     that runs the frozen copy of this script every 10 s. Unless the state
#     file says the deploy finished, and the deploy unit is gone or past its
#     deadline, the guard restores the pinned old binary and config and
#     restarts the engine. It is a real unit file pair, enabled, so it also
#     comes back if the user manager restarts. Nothing is changed unless the
#     timer is confirmed active.
#  5. Adds `claude: server-side-fallback: true` to the config (keeps v8.0.64's
#     automatic refusal fallback, which v8.0.77 makes opt-in), then installs
#     the candidate by copy-beside-and-rename and restarts the engine.
#  6. Checks the new process: unit active, /proc/PID/exe sha is the candidate,
#     /healthz, /v1/models, /pool/usage, one real Claude and one real Codex
#     request, then 30 s with the same PID and no restarts.
#  7. On success: re-verifies the installed and running sha, records success,
#     disarms the guard. On failure: restores the old binary and config,
#     restarts, verifies, disarms. If that restore fails, the guard stays
#     armed and retries (3 attempts, then it records needs-human and stops).
#
# No management endpoint is called, so a deploy uses zero failed management
# logins (the old script's X-CPA-VERSION probe used one per process).
# Response bodies, headers and keys are never logged: requests log a status
# code and a fixed classification only.
#
# ---------------------------------------------------------------- time bounds
#
# Every external step has a timeout (systemctl 10 s, sha256 15 s, each copy
# 30 s, curl 5-60 s). Summed from those bounds, the success path is at most:
#   preflight + baseline ......... 220 s   (2 sha, 2 banners, unit props, 5+10+60+60 s requests)
#   backups + arm guard .......... 135 s
#   config edit + swap + restart . 110 s
#   checks on the new binary ..... 315 s   (100 s to come up, 10+5+60+60 s requests, 30 s stable)
#   verify + disarm ..............  70 s
#   total ........................ 850 s
# The deploy unit runs with RuntimeMaxSec=900, so systemd stops it at 900 s
# whatever happens (TimeoutStopSec=10, then SIGKILL). A failing deploy's own
# rollback adds up to 365 s; if that would pass 900 s, systemd cuts the deploy
# and the guard finishes the restore.
# The guard restores unless success is recorded by deploy start + 930 s, or as
# soon as the deploy unit is gone without success (checked every 10 s). One
# guard attempt is at most 480 s (TimeoutStartSec; summed bound 470 s); it
# makes at most 3 attempts, then records needs-human and stops.
# Measured in rehearsal: success 31 s; recovery 11-21 s after a killed deploy.
# Worst case to a final state: 900 + 10 + 10 + 3 x 490 = 2390 s (about 40 min).
#
# Exit codes: 0 deployed, or already deployed; 1 the candidate failed and the
# old binary is back and healthy; 2 refused or aborted with nothing changed;
# 3 rollback failed in this shell, the guard is armed and retrying;
# 4 another deploy holds the lock.
#
# ---------------------------------------------------------------- rehearsal
#
# VPE_REHEARSAL_DIR=/tmp/<dir> switches every path, unit name, guard name and
# port to throwaway values read from <dir>/rehearsal.conf, and enables fault
# hooks read from <dir>/faults. It refuses to run if any of them resolves to
# the live directory, unit or port. Without it, the pins below are constants
# and no environment variable changes them.
set -uo pipefail
umask 077

DEPLOY_VERSION=v8.0.77-rhys.1
RUNTIME_MAX=900          # RuntimeMaxSec of the deploy unit
GUARD_GRACE=30           # guard deadline = deploy start + RUNTIME_MAX + GUARD_GRACE
GUARD_ATTEMPTS=3
SC_T=10                  # timeout for each systemctl call
COPY_T=30                # timeout for each binary/config copy
HEALTH_WAIT=60           # seconds for a restarted process to come up and pass /healthz
RESTORE_WAIT=45
REQUEST_TIMEOUT=60       # per real model request
STABLE_SECONDS=30
RESTORE_STABLE=10
CLAUDE_MODEL=claude-haiku-4-5-20251001
CODEX_MODEL=gpt-5.6-luna
CONFIG_MARK='# Added by deploy-v8.0.77-rhys.1: keep v8.0.64 refusal fallback (Opus 5.5 -> Opus 4.8, Fable 5.1 -> Opus 5).'

die_early() { printf 'ERROR: %s\n' "$*" >&2; exit 2; }

if [[ -n "${VPE_REHEARSAL_DIR:-}" ]]; then
	REHEARSAL=1
	R="$(realpath -e "$VPE_REHEARSAL_DIR" 2>/dev/null)" || die_early "rehearsal dir missing"
	[[ "$R" == /tmp/* ]] || die_early "rehearsal dir must be under /tmp"
	rconf() { awk -F= -v k="$1" '$1 == k { sub(/^[^=]*=/, ""); print; exit }' "$R/rehearsal.conf"; }
	UNIT="$(rconf unit)"; PORT="$(rconf port)"
	OLD_VERSION="$(rconf old_version)"; OLD_SHA="$(rconf old_sha)"
	NEW_VERSION="$(rconf new_version)"; NEW_SHA="$(rconf new_sha)"
	LIVE_DIR="$R/live"
	NEW_BIN="$(rconf new_bin)"
	STATE_DIR="$R/state"
	GUARD="vpe-rh-guard-$(basename "$R")"
	RUNTIME_MAX="$(rconf runtime_max)"; RUNTIME_MAX="${RUNTIME_MAX:-900}"
	DEPLOY_PREFIX="vpe-rh-deploy-"
	for v in UNIT PORT OLD_VERSION OLD_SHA NEW_VERSION NEW_SHA NEW_BIN; do [[ -n "${!v}" ]] || die_early "rehearsal.conf lacks $v"; done
	[[ "$UNIT" != vibeproxy-engine* && "$PORT" != 8318 ]] || die_early "rehearsal points at the live unit or port"
	[[ "$(realpath -m "$LIVE_DIR")" != "$(realpath -m "$HOME/.cli-proxy-api")" && "$NEW_BIN" != "$HOME/.cli-proxy-api"* ]] || die_early "rehearsal points at the live directory"
else
	REHEARSAL=0
	UNIT=vibeproxy-engine
	PORT=8318
	OLD_VERSION=v8.0.64-rhys.2
	OLD_SHA=6f5add08917cbaf9efeb4230674bc194f0b36076dbf67988fd0d29f8d255d9e7
	NEW_VERSION=v8.0.77-rhys.1
	NEW_SHA=650b70ba0d36daba9f15e7fb3c0368d90abb08c336db4553c5dfe06cc52e3c00
	LIVE_DIR="$HOME/.cli-proxy-api"
	NEW_BIN="$LIVE_DIR/cli-proxy-api-plus.v8.0.77-rhys.1"
	STATE_DIR="$HOME/.local/state/vibeproxy-deploy/$DEPLOY_VERSION"
	GUARD=vibeproxy-deploy-guard
	DEPLOY_PREFIX=vibeproxy-deploy-
fi
LIVE_BIN="$LIVE_DIR/cli-proxy-api-plus"
CONFIG="$LIVE_DIR/merged-config.yaml"
BASE="http://127.0.0.1:$PORT"
STATE="$STATE_DIR/state"
LOCK="$STATE_DIR/lock"
LOG_DIR="$STATE_DIR/logs"
UNIT_DIR="$HOME/.config/systemd/user"
BIN_BACKUP="$STATE_DIR/cli-proxy-api-plus.$OLD_VERSION"
CONFIG_BACKUP="$STATE_DIR/merged-config.yaml.before"
mkdir -p "$LOG_DIR" && chmod 700 "$STATE_DIR" "$LOG_DIR" || die_early "cannot create $STATE_DIR"

# ---------------------------------------------------------------- helpers

LOG=/dev/stderr
log() { printf '%s %s\n' "$(date -u +%H:%M:%SZ)" "$*" >>"$LOG"; }
sc() { timeout "$SC_T" systemctl --user "$@"; }
unit_prop() { sc show "$UNIT" -p "$1" --value 2>/dev/null; }
sha_of() { timeout 15 sha256sum "$1" 2>/dev/null | awk '{print $1}'; }
banner() { { timeout 10 "$1" -h 2>&1 || true; } | sed -n 1p; }
now() { date +%s; }

fault() { # name -> value, rehearsal only
	[[ "$REHEARSAL" == 1 && -f "$R/faults" ]] || return 0
	awk -F= -v k="$1" '$1 == k { sub(/^[^=]*=/, ""); print; exit }' "$R/faults"
}
pause_hook() { # rehearsal: stop here so the harness can kill this unit
	[[ "$(fault pause_at)" == "$1" ]] || return 0
	log "  [rehearsal] paused at $1"
	: >"$R/paused-$1"
	sleep 300
}

state_get() { [[ -f "$STATE" ]] && awk -F= -v k="$1" '$1 == k { sub(/^[^=]*=/, ""); print; exit }' "$STATE"; }
state_set() { # key value [key value ...], atomic rewrite
	local tmp="$STATE.tmp.$$" args=("$@")
	{
		[[ -f "$STATE" ]] && awk -F= -v keys="$(printf '%s ' "${args[@]}")" '
			BEGIN { n = split(keys, a, " "); for (i = 1; i <= n; i += 2) skip[a[i]] = 1 }
			!($1 in skip)' "$STATE"
		local i
		for ((i = 0; i < ${#args[@]}; i += 2)); do printf '%s=%s\n' "${args[i]}" "${args[i+1]}"; done
	} >"$tmp" && sync "$tmp" 2>/dev/null; mv -f "$tmp" "$STATE"
}
is_final() { [[ "$1" == succeeded || "$1" == rolled-back || "$1" == recovered || "$1" == needs-human || "$1" == nothing-changed ]]; }

# Copy src to a temp name beside dst, verify sha, fsync, rename over dst.
install_file() { # src dst expect_sha mode
	local tmp
	tmp="$(dirname "$2")/.$(basename "$2").deploy-tmp.$$"
	timeout "$COPY_T" cp "$1" "$tmp" || { rm -f "$tmp"; log "  copy of $1 failed"; return 1; }
	chmod "$4" "$tmp"
	[[ "$(sha_of "$tmp")" == "$3" ]] || { rm -f "$tmp"; log "  copy of $1 has the wrong sha"; return 1; }
	sync "$tmp" 2>/dev/null
	mv -f "$tmp" "$2" || { rm -f "$tmp"; log "  rename onto $2 failed"; return 1; }
}

# First entry of the top-level api-keys list. Never printed.
read_api_key() {
	awk '
		/^api-keys:[[:space:]]*$/ { inlist = 1; next }
		inlist && /^[[:space:]]*-[[:space:]]*/ {
			sub(/^[[:space:]]*-[[:space:]]*/, ""); gsub(/["\047]/, ""); sub(/[[:space:]]+#.*$/, "")
			sub(/[[:space:]]+$/, ""); print; exit
		}
		inlist && /^[^[:space:]]/ { exit }
	' "$CONFIG"
}
# Header files keep the key off curl's command line (visible in ps).
make_auth_headers() {
	local key
	key="$(read_api_key)"
	[[ -n "$key" ]] || return 1
	AUTH_BEARER="$STATE_DIR/.h-bearer.$$"; AUTH_XKEY="$STATE_DIR/.h-xkey.$$"
	printf 'Authorization: Bearer %s\n' "$key" >"$AUTH_BEARER"
	printf 'x-api-key: %s\n' "$key" >"$AUTH_XKEY"
}
drop_auth_headers() { rm -f "${AUTH_BEARER:-}" "${AUTH_XKEY:-}"; }

http_code() { curl -s -o /dev/null -w '%{http_code}' --max-time "${2:-5}" "$1" 2>/dev/null || true; }

# Status code and a fixed classification only; never a body or header.
classify() {
	case "$1" in
		000|"") echo "no response (timeout or connection refused)" ;;
		2??) echo "unexpected response shape" ;;
		401|403) echo "rejected: auth" ;;
		429) echo "rate limited or quota" ;;
		4??) echo "client error" ;;
		5??) echo "server or upstream error" ;;
		*) echo "unexpected status" ;;
	esac
}

check_models() {
	local out code ok
	out="$(mktemp "$STATE_DIR/.body.XXXXXX")"
	code="$(curl -s -o "$out" -w '%{http_code}' --max-time 10 -H "@$AUTH_BEARER" "$BASE/v1/models" 2>/dev/null)" || true
	ok="$(jq -r --arg c "$CLAUDE_MODEL" --arg x "$CODEX_MODEL" \
		'[.data[].id] as $ids | (($ids | index($c)) != null and ($ids | index($x)) != null)' "$out" 2>/dev/null)"
	rm -f "$out"
	if [[ "$code" == 200 && "$ok" == true ]]; then log "  models: 200, both test models listed"; return 0; fi
	log "  models: FAILED http=$code ($( [[ "$code" == 200 ]] && echo "a test model is missing" || classify "$code"))"; return 1
}

check_claude() {
	local out code ok
	out="$(mktemp "$STATE_DIR/.body.XXXXXX")"
	code="$(curl -s -o "$out" -w '%{http_code}' --max-time "$REQUEST_TIMEOUT" \
		-H "@$AUTH_XKEY" -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' \
		"$BASE/v1/messages" \
		-d "{\"model\":\"$CLAUDE_MODEL\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with the word OK.\"}]}" 2>/dev/null)" || true
	ok="$(jq -r '(.content | length) > 0' "$out" 2>/dev/null)"; rm -f "$out"
	if [[ "$code" == 200 && "$ok" == true ]]; then log "  claude: 200, answered"; return 0; fi
	log "  claude: FAILED http=${code:-000} ($(classify "$code"))"; return 1
}

check_codex() {
	local out code ok
	out="$(mktemp "$STATE_DIR/.body.XXXXXX")"
	code="$(curl -s -o "$out" -w '%{http_code}' --max-time "$REQUEST_TIMEOUT" \
		-H "@$AUTH_BEARER" -H 'content-type: application/json' \
		"$BASE/v1/responses" \
		-d "{\"model\":\"$CODEX_MODEL\",\"input\":\"Reply with the word OK.\",\"stream\":false}" 2>/dev/null)" || true
	ok="$(jq -r '.status == "completed" and ((.output | length) > 0)' "$out" 2>/dev/null)"; rm -f "$out"
	if [[ "$code" == 200 && "$ok" == true ]]; then log "  codex: 200, completed"; return 0; fi
	log "  codex: FAILED http=${code:-000} ($(classify "$code"))"; return 1
}

# Wait until the unit is active, its MainPID runs expect_sha and /healthz is 200.
# Sets RUN_PID. Returns 1 after `wait` seconds.
wait_up() { # expect_sha wait
	local deadline=$(( $(now) + $2 )) pid
	while :; do
		pid="$(unit_prop MainPID)"
		if [[ "$(unit_prop ActiveState)" == active && -n "$pid" && "$pid" != 0 ]] &&
			[[ "$(sha_of "/proc/$pid/exe")" == "$1" && "$(http_code "$BASE/healthz")" == 200 ]]; then
			RUN_PID="$pid"; return 0
		fi
		(( $(now) < deadline )) || return 1
		sleep 2
	done
}

stable_for() { # seconds
	local pid="$RUN_PID" restarts
	restarts="$(unit_prop NRestarts)"
	sleep "$1"
	[[ "$(unit_prop MainPID)" == "$pid" && "$(unit_prop NRestarts)" == "$restarts" && "$(unit_prop ActiveState)" == active ]] ||
		{ log "  not stable: pid $(unit_prop MainPID) (was $pid), NRestarts $(unit_prop NRestarts) (was $restarts)"; return 1; }
	log "  stable for ${1}s (pid $pid)"
}

check_new() {
	log "checking $UNIT on $NEW_VERSION (sha ${NEW_SHA:0:12})"
	wait_up "$NEW_SHA" "$HEALTH_WAIT" || { log "  not active with sha ${NEW_SHA:0:12} and /healthz 200 within ${HEALTH_WAIT}s"; return 1; }
	log "  pid $RUN_PID up, exe sha matches, /healthz 200"
	check_models || return 1
	[[ "$(http_code "$BASE/pool/usage")" == 200 ]] || { log "  /pool/usage (read by the usage dashboard) not 200"; return 1; }
	log "  /pool/usage 200"
	pause_hook checks
	check_claude || return 1
	check_codex || return 1
	stable_for "$STABLE_SECONDS"
}

# ---------------------------------------------------------------- guard units

guard_armed() { [[ "$(sc is-active "$GUARD.timer" 2>/dev/null)" == active ]]; }

arm_guard() { # frozen_script
	local env_line=""
	[[ "$REHEARSAL" == 1 ]] && env_line="Environment=VPE_REHEARSAL_DIR=$R"
	mkdir -p "$UNIT_DIR"
	cat >"$UNIT_DIR/$GUARD.service" <<-EOF
	[Unit]
	Description=vibeproxy deploy guard: restore $OLD_VERSION unless the $NEW_VERSION deploy recorded success
	[Service]
	Type=oneshot
	TimeoutStartSec=480
	$env_line
	ExecStart=/bin/bash $1 guard
	EOF
	cat >"$UNIT_DIR/$GUARD.timer" <<-EOF
	[Unit]
	Description=Tick for $GUARD.service (removed when the deploy is resolved)
	[Timer]
	OnActiveSec=10
	OnUnitInactiveSec=10
	AccuracySec=1s
	[Install]
	WantedBy=timers.target
	EOF
	sc daemon-reload && sc enable --now "$GUARD.timer" >/dev/null 2>&1 && guard_armed
}

disarm_guard() {
	# A killed deploy can leave its key-header and response-body temp files.
	rm -f "$STATE_DIR"/.h-bearer.* "$STATE_DIR"/.h-xkey.* "$STATE_DIR"/.body.*
	sc disable --now "$GUARD.timer" >/dev/null 2>&1
	rm -f "$UNIT_DIR/$GUARD.timer" "$UNIT_DIR/$GUARD.service"
	sc daemon-reload
	sc reset-failed "$GUARD.service" "$GUARD.timer" >/dev/null 2>&1
	if guard_armed; then log "  WARNING: guard timer still active after disarm"; return 1; fi
	log "  guard disarmed"
}

# ---------------------------------------------------------------- restore

# Idempotent: puts back the pinned old binary and the pre-deploy config, then
# restarts only if the running process is not already the healthy old binary.
restore_old() {
	local cfg_before cur
	cfg_before="$(state_get config_sha_before)"
	if [[ "$(sha_of "$LIVE_BIN")" != "$OLD_SHA" ]]; then
		install_file "$BIN_BACKUP" "$LIVE_BIN" "$OLD_SHA" 755 || return 1
		log "  restored $OLD_VERSION binary"
	fi
	cur="$(sha_of "$CONFIG")"
	if [[ -n "$cfg_before" && "$cur" != "$cfg_before" ]]; then
		install_file "$CONFIG_BACKUP" "$CONFIG" "$cfg_before" 600 || return 1
		log "  restored the pre-deploy config"
	fi
	local pid
	pid="$(unit_prop MainPID)"
	if [[ "$(unit_prop ActiveState)" == active && "$pid" != 0 && "$(sha_of "/proc/$pid/exe")" == "$OLD_SHA" &&
		"$(http_code "$BASE/healthz")" == 200 ]]; then
		RUN_PID="$pid"
		log "  $UNIT already runs $OLD_VERSION and answers; no restart needed"
	else
		if [[ "$(fault restore_fail_always)" == 1 ]]; then
			log "  [rehearsal] injected failure: restart of $UNIT fails (every time)"
			return 1
		fi
		if [[ "$(fault restore_fail_once)" == 1 && ! -f "$R/restore-failed-once" ]]; then
			: >"$R/restore-failed-once"
			log "  [rehearsal] injected failure: restart of $UNIT fails"
			return 1
		fi
		sc reset-failed "$UNIT" >/dev/null 2>&1
		sc restart --no-block "$UNIT" || { log "  systemctl restart $UNIT failed"; return 1; }
		wait_up "$OLD_SHA" "$RESTORE_WAIT" || { log "  $OLD_VERSION not active and answering within ${RESTORE_WAIT}s"; return 1; }
		log "  pid $RUN_PID runs $OLD_VERSION, /healthz 200"
	fi
	if make_auth_headers; then check_models; local m=$?; drop_auth_headers; (( m == 0 )) || return 1; fi
	stable_for "$RESTORE_STABLE"
}

# ---------------------------------------------------------------- commands

cmd_status() {
	if [[ ! -f "$STATE" ]]; then echo "no deploy recorded in $STATE_DIR"; exit 0; fi
	grep -E '^(phase|deploy_unit|started|deadline|log|guard_attempts|finished)=' "$STATE"
	echo "guard timer: $(sc is-active "$GUARD.timer" 2>/dev/null)"
	echo "installed sha: $(sha_of "$LIVE_BIN" | cut -c1-12) (old ${OLD_SHA:0:12}, new ${NEW_SHA:0:12})"
}

cmd_launch() {
	exec 9>>"$LOCK"
	flock -n 9 || { echo "REFUSED: a deploy is already running (lock held); nothing started" >&2; exit 4; }
	local phase
	phase="$(state_get phase)"
	if [[ -n "$phase" ]] && ! is_final "$phase"; then
		echo "REFUSED: previous deploy unresolved (phase=$phase); see: bash $0 status" >&2; exit 2
	fi
	flock -u 9
	local ts frozen unit
	ts="$(date -u +%Y%m%dT%H%M%SZ)-$$"
	frozen="$STATE_DIR/deploy-$ts.sh"
	cp "${BASH_SOURCE[0]}" "$frozen.tmp" && chmod 700 "$frozen.tmp" && mv -f "$frozen.tmp" "$frozen" || { echo "cannot freeze script" >&2; exit 2; }
	unit="${DEPLOY_PREFIX}$(date +%s)-$$"
	local envarg=()
	[[ "$REHEARSAL" == 1 ]] && envarg=(-E "VPE_REHEARSAL_DIR=$R")
	timeout 20 systemd-run --user --quiet --collect --unit="$unit" \
		-p RuntimeMaxSec="$RUNTIME_MAX" -p TimeoutStopSec=10 "${envarg[@]}" \
		/bin/bash "$frozen" deploy "$ts" || { echo "systemd-run failed" >&2; exit 2; }
	echo "launched $unit; log: $LOG_DIR/deploy-$ts.log; status: bash $0 status"
}

cmd_deploy() { # mode ts
	local mode="$1" ts="$2" my_unit phase
	LOG="$LOG_DIR/deploy-$ts.log"
	exec 9>>"$LOCK"
	my_unit="$(sed -n 's#^0::.*/##p' /proc/self/cgroup)"; my_unit="${my_unit%.service}"
	log "mode=$mode unit=$UNIT port=$PORT deploy-unit=${my_unit:-none} rehearsal=$REHEARSAL"
	if ! flock -n 9; then log "REFUSED: another deploy holds the lock; nothing changed"; log "RESULT: refused (concurrent deploy)"; exit 4; fi

	phase="$(state_get phase)"
	if [[ "$phase" == succeeded && "$(sha_of "$LIVE_BIN")" == "$NEW_SHA" ]]; then
		local pid; pid="$(unit_prop MainPID)"
		if [[ "$(sha_of "/proc/$pid/exe")" == "$NEW_SHA" ]]; then
			log "$NEW_VERSION is already installed and running (success recorded $(state_get finished)); nothing to do"
			log "RESULT: already deployed; nothing changed"; exit 0
		fi
	fi
	if [[ -n "$phase" ]] && ! is_final "$phase"; then
		log "REFUSED: previous deploy unresolved (phase=$phase, guard timer $(sc is-active "$GUARD.timer" 2>/dev/null))"
		log "RESULT: refused (unresolved previous deploy)"; exit 2
	fi
	die_pre() { drop_auth_headers; log "ABORT (nothing changed): $*"; log "RESULT: aborted; nothing changed"; exit 2; }

	# -------- preflight: exact pins, no inference
	command -v jq >/dev/null || die_pre "jq not installed"
	[[ -f "$LIVE_BIN" && -f "$NEW_BIN" && -f "$CONFIG" ]] || die_pre "a required file is missing"
	local live_sha new_sha run_pid run_sha
	live_sha="$(sha_of "$LIVE_BIN")"; new_sha="$(sha_of "$NEW_BIN")"
	[[ "$live_sha" == "$OLD_SHA" ]] || die_pre "installed binary sha ${live_sha:0:12} is not the pinned $OLD_VERSION (${OLD_SHA:0:12})"
	[[ "$new_sha" == "$NEW_SHA" ]] || die_pre "candidate sha ${new_sha:0:12} is not the pinned $NEW_VERSION (${NEW_SHA:0:12})"
	[[ "$(banner "$LIVE_BIN")" == *"Version: $OLD_VERSION,"* ]] || die_pre "installed binary does not report $OLD_VERSION"
	[[ "$(banner "$NEW_BIN")" == *"Version: $NEW_VERSION,"* ]] || die_pre "candidate does not report $NEW_VERSION"
	[[ "$(unit_prop ActiveState)" == active ]] || die_pre "$UNIT is not active"
	run_pid="$(unit_prop MainPID)"; run_sha="$(sha_of "/proc/$run_pid/exe")"
	[[ "$run_sha" == "$OLD_SHA" ]] || die_pre "running pid $run_pid is sha ${run_sha:0:12}, not the pinned $OLD_VERSION"
	if grep -qE '^(claude|upstream):' "$CONFIG"; then die_pre "config already has a top-level claude: or upstream: block; refusing to edit it"; fi
	make_auth_headers || die_pre "no api-keys entry in the config"
	log "pins ok: installed and running $OLD_VERSION ${OLD_SHA:0:12}; candidate $NEW_VERSION ${NEW_SHA:0:12}"

	log "baseline on the running $OLD_VERSION"
	[[ "$(http_code "$BASE/healthz")" == 200 ]] || die_pre "baseline /healthz failed"
	check_models || die_pre "baseline models listing failed"
	check_claude && check_codex || die_pre "baseline real requests failed on the old binary; fix that first"
	if [[ "$mode" == check-only ]]; then drop_auth_headers; log "RESULT: check-only passed; nothing changed"; exit 0; fi

	# -------- arm recovery before any change
	[[ "${BASH_SOURCE[0]}" == "$STATE_DIR"/deploy-*.sh ]] || die_pre "deploy must run from a frozen copy; use: launch"
	[[ "$my_unit" == "$DEPLOY_PREFIX"* ]] || die_pre "deploy must run in its own ${DEPLOY_PREFIX}* unit; use: launch"
	[[ -f "$STATE" ]] && mv -f "$STATE" "$STATE.prev-$ts"
	local cfg_before started
	install_file "$LIVE_BIN" "$BIN_BACKUP" "$OLD_SHA" 755 || die_pre "binary backup failed"
	cfg_before="$(sha_of "$CONFIG")"
	install_file "$CONFIG" "$CONFIG_BACKUP" "$cfg_before" 600 || die_pre "config backup failed"
	started="$(now)"
	state_set phase armed deploy_unit "$my_unit" started "$started" deadline "$((started + RUNTIME_MAX + GUARD_GRACE))" \
		log "$LOG" old_sha "$OLD_SHA" new_sha "$NEW_SHA" config_sha_before "$cfg_before" guard_attempts 0
	if ! arm_guard "${BASH_SOURCE[0]}"; then
		disarm_guard; state_set phase nothing-changed; die_pre "could not arm the guard timer"
	fi
	log "backups in $STATE_DIR; guard $GUARD.timer armed (deadline $(date -u -d "@$((started + RUNTIME_MAX + GUARD_GRACE))" +%H:%M:%SZ))"

	trap 'log "deploy shell got SIGTERM/SIGINT; the guard will restore $OLD_VERSION if needed"; drop_auth_headers; exit 143' TERM INT

	# -------- config: keep today's refusal fallback behaviour
	local cfg_tmp="$LIVE_DIR/.merged-config.yaml.deploy-tmp.$$"
	if ! { timeout "$COPY_T" cp -p "$CONFIG" "$cfg_tmp" && printf '\n%s\nclaude:\n  server-side-fallback: true\n' "$CONFIG_MARK" >>"$cfg_tmp" &&
		chmod 600 "$cfg_tmp" && sync "$cfg_tmp" && mv -f "$cfg_tmp" "$CONFIG"; }; then
		rm -f "$cfg_tmp"; log "config edit failed"; rollback_and_exit
	fi
	state_set phase swapping config_sha_after "$(sha_of "$CONFIG")"
	log "config: added claude.server-side-fallback: true"

	# -------- swap and restart
	install_file "$NEW_BIN" "$LIVE_BIN" "$NEW_SHA" 755 || { log "install of the candidate failed"; rollback_and_exit; }
	state_set phase swapped
	log "installed $NEW_VERSION at $LIVE_BIN"
	pause_hook swap
	sc reset-failed "$UNIT" >/dev/null 2>&1
	sc restart --no-block "$UNIT" || { log "systemctl restart failed"; rollback_and_exit; }
	log "restart of $UNIT queued"

	if check_new; then
		# Record success only after re-verifying what is installed and running.
		local pid; pid="$(unit_prop MainPID)"
		if [[ "$(sha_of "$LIVE_BIN")" == "$NEW_SHA" && "$(sha_of "/proc/$pid/exe")" == "$NEW_SHA" ]]; then
			state_set phase succeeded finished "$(date -u +%FT%TZ)"
			disarm_guard
			drop_auth_headers
			log "RESULT: deployed $NEW_VERSION; healthy. Backups: $BIN_BACKUP, $CONFIG_BACKUP"
			exit 0
		fi
		log "re-verification before recording success failed"
	fi
	log "the candidate failed its checks"
	rollback_and_exit
}

rollback_and_exit() {
	state_set phase rolling-back
	log "ROLLBACK to $OLD_VERSION"
	drop_auth_headers
	if restore_old; then
		state_set phase rolled-back finished "$(date -u +%FT%TZ)"
		disarm_guard
		log "RESULT: rolled back; $OLD_VERSION is live and healthy. Candidate left at $NEW_BIN"
		exit 1
	fi
	state_set phase rollback-failed
	log "RESULT: rollback failed in the deploy shell; the guard ($GUARD.timer) stays armed and retries"
	exit 3
}

cmd_guard() {
	[[ -f "$STATE" ]] || { LOG="$LOG_DIR/guard.log"; log "guard: no state file; disarming"; disarm_guard; exit 0; }
	LOG="$(state_get log)"; [[ -n "$LOG" ]] || LOG="$LOG_DIR/guard.log"
	local phase deploy_unit deadline active
	phase="$(state_get phase)"
	if is_final "$phase"; then log "guard: deploy resolved (phase=$phase); disarming"; disarm_guard; exit 0; fi
	deploy_unit="$(state_get deploy_unit)"; deadline="$(state_get deadline)"
	active="$(sc is-active "$deploy_unit" 2>/dev/null)"
	if [[ "$active" =~ ^(active|activating|deactivating|reloading)$ ]] && (( $(now) < deadline )); then exit 0; fi

	if [[ "$active" =~ ^(active|activating|deactivating|reloading)$ ]]; then
		log "guard: deadline passed with $deploy_unit still $active; stopping it"
		timeout 25 systemctl --user stop "$deploy_unit" || timeout 10 systemctl --user kill --signal=KILL "$deploy_unit"
	else
		log "guard: deploy unit $deploy_unit is ${active:-gone} and phase=$phase (no success recorded)"
	fi
	exec 9>>"$LOCK"
	flock -w 60 9 || { log "guard: could not take the lock within 60s; will retry"; exit 1; }
	phase="$(state_get phase)"
	if is_final "$phase"; then log "guard: deploy resolved meanwhile (phase=$phase); disarming"; disarm_guard; exit 0; fi

	local attempts=$(( $(state_get guard_attempts) + 1 ))
	state_set guard_attempts "$attempts"
	log "guard: restore attempt $attempts/$GUARD_ATTEMPTS"
	if restore_old; then
		state_set phase recovered finished "$(date -u +%FT%TZ)"
		disarm_guard
		log "RESULT: guard restored $OLD_VERSION; live and healthy"
		exit 0
	fi
	if (( attempts >= GUARD_ATTEMPTS )); then
		state_set phase needs-human
		log "RESULT: NEEDS A HUMAN. Guard failed $attempts times. Installed sha $(sha_of "$LIVE_BIN" | cut -c1-12), running sha $(sha_of "/proc/$(unit_prop MainPID)/exe" | cut -c1-12) (old ${OLD_SHA:0:12}); old binary at $BIN_BACKUP; config backup $CONFIG_BACKUP"
		disarm_guard
		exit 1
	fi
	log "guard: attempt $attempts failed; retrying on the next tick"
	exit 1
}

case "${1:-}" in
	launch) cmd_launch ;;
	deploy) [[ -n "${2:-}" ]] || die_early "use: launch"; cmd_deploy deploy "$2" ;;
	check-only) ts="$(date -u +%Y%m%dT%H%M%SZ)-check-only"; cmd_deploy check-only "$ts" ;;
	guard) cmd_guard ;;
	status) cmd_status ;;
	*) echo "usage: $0 check-only | launch | status" >&2; exit 2 ;;
esac
