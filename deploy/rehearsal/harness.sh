#!/usr/bin/env bash
# Rehearsal harness for deploy-v8.0.77-rhys.1.sh. Throwaway units and files only.
# Everything lives under $B (default /tmp/vpe-fx): run dirs $B/run-<name>,
# engine units vpe-fx-engine-<name>, guard units vpe-rh-guard-run-<name>.
set -uo pipefail
SCRIPT=/home/rhys/agent-workspace/.rifts/vibeproxy-engine/rhys-v8077/deploy/deploy-v8.0.77-rhys.1.sh
B="${VPE_FX_BASE:-/tmp/vpe-fx}"
STUB=$B/stub
UD="$HOME/.config/systemd/user"
OLD_SHA="$(sha256sum $STUB/bin-old | cut -d' ' -f1)"
NEW_SHA="$(sha256sum $STUB/bin-new | cut -d' ' -f1)"
FINAL_RE='^(succeeded|rolled-back|recovered|needs-human|nothing-changed)$'

setup() { # name candidate port
	local n="$1" cand="$2" port="$3" R="$B/run-$1"
	mkdir -p "$R/live" "$R/cand" "$R/state"; chmod 700 "$R"
	cp "$STUB/bin-old" "$R/live/cli-proxy-api-plus"
	cp "$STUB/bin-$cand" "$R/cand/cli-proxy-api-plus.v8.0.77-rhys.1"
	printf 'api-keys:\n  - "rehearsal-fake-key"\nhost: 127.0.0.1\nport: %s\ndebug: false\n' "$port" >"$R/live/merged-config.yaml"
	chmod 600 "$R/live/merged-config.yaml"
	sha256sum "$R/live/merged-config.yaml" | cut -d' ' -f1 >"$R/config.sha0"
	cat >"$R/rehearsal.conf" <<-EOF
	unit=vpe-fx-engine-$n
	port=$port
	old_version=v8.0.64-rhys.2
	old_sha=$OLD_SHA
	new_version=v8.0.77-rhys.1
	new_sha=$(sha256sum "$STUB/bin-$cand" | cut -d' ' -f1)
	new_bin=$R/cand/cli-proxy-api-plus.v8.0.77-rhys.1
	EOF
	cat >"$UD/vpe-fx-engine-$n.service" <<-EOF
	[Unit]
	Description=Rehearsal stand-in engine ($n)
	[Service]
	WorkingDirectory=$R/live
	ExecStart=$R/live/cli-proxy-api-plus -config $R/live/merged-config.yaml
	Restart=always
	RestartSec=3
	StandardOutput=append:$R/live/engine.log
	StandardError=append:$R/live/engine.log
	EOF
	systemctl --user daemon-reload
	systemctl --user start "vpe-fx-engine-$n"
	timeout 20 bash -c "until curl -sf -o /dev/null http://127.0.0.1:$port/healthz; do sleep 0.5; done" || echo "SETUP FAILED $n"
}

wait_for() { # seconds description command...
	local t="$1" d="$2"; shift 2
	timeout "$t" bash -c "until $*; do sleep 1; done" || { echo "TIMEOUT waiting for $d"; return 1; }
}
st() { awk -F= -v k="$2" '$1==k{print $2}' "$B/run-$1/state/state" 2>/dev/null; }
phase() { st "$1" phase; }
fin() { wait_for "$2" "$1 final" "[[ \"\$(awk -F= '\$1==\"phase\"{print \$2}' $B/run-$1/state/state 2>/dev/null)\" =~ $FINAL_RE ]]"; }
paused() { wait_for "${3:-90}" "$1 paused at $2" "[[ -f $B/run-$1/paused-$2 ]]"; }
resume() { rm -f "$B/run-$1/paused-$2"; }
kill_deploy() { systemctl --user kill --signal="${2:-KILL}" "$(st "$1" deploy_unit)"; rm -f "$B"/run-"$1"/paused-*; }
kill_guard() { systemctl --user kill --signal="${2:-KILL}" "vpe-rh-guard-run-$1.service"; rm -f "$B"/run-"$1"/paused-*; }
corrupt_backup() { printf 'corrupted by rehearsal\n' >>"$B/run-$1/state/merged-config.yaml.before"; }

report() { # name expect(old|new)
	local n="$1" R="$B/run-$1" pid exe port want
	port="$(awk -F= '$1=="port"{print $2}' "$R/rehearsal.conf")"
	pid="$(systemctl --user show "vpe-fx-engine-$n" -p MainPID --value)"
	exe="$(sha256sum "/proc/$pid/exe" 2>/dev/null | cut -d' ' -f1)"
	[[ "$2" == old ]] && want="$OLD_SHA" || want="$(awk -F= '$1=="new_sha"{print $2}' "$R/rehearsal.conf")"
	echo "--- outcome [$n]"
	echo "phase=$(phase "$n") guard_attempts=$(st "$n" guard_attempts) final_action=$(st "$n" final_action) final_check=$(st "$n" final_check) config_restored=$(st "$n" config_restored)"
	echo "serving: pid=$pid exe=${exe:0:12} expected-$2=${want:0:12} match=$([[ "$exe" == "$want" ]] && echo yes || echo NO) healthz=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$port/healthz")"
	echo "installed=$(sha256sum "$R/live/cli-proxy-api-plus" | cut -c1-12) config=$( [[ "$(sha256sum "$R/live/merged-config.yaml" | cut -d' ' -f1)" == "$(cat "$R/config.sha0")" ]] && echo original || echo "edited ($(grep -c server-side-fallback "$R/live/merged-config.yaml") fallback line)")"
	echo "guard unit files left: $(ls "$UD" | grep -c "vpe-rh-guard-run-$n\.") ; guard timer: $(systemctl --user is-active "vpe-rh-guard-run-$n.timer" 2>/dev/null) ; secrets in logs: $(grep -rl 'STANDIN-SECRET\|rehearsal-fake-key' "$R/state/logs" 2>/dev/null | wc -l) files"
	echo "attempt lines: $(grep -ho 'restore attempt [0-9]*/[0-9]*' "$R"/state/logs/*.log | tr '\n' ' ')"
}

launch() { VPE_REHEARSAL_DIR="$B/run-$1" bash "$SCRIPT" launch; }

teardown() {
	local n="$1"
	local u
	# One unit per call: systemctl stops nothing if any named unit is not loaded.
	systemctl --user disable --now "vpe-rh-guard-run-$n.timer" 2>/dev/null
	for u in "vpe-rh-guard-run-$n.timer" "vpe-rh-guard-run-$n.service" "vpe-fx-engine-$n.service"; do
		systemctl --user stop "$u" 2>/dev/null
	done
	rm -f "$UD/vpe-fx-engine-$n.service" "$UD/vpe-rh-guard-run-$n.service" "$UD/vpe-rh-guard-run-$n.timer" \
		"$UD/timers.target.wants/vpe-rh-guard-run-$n.timer"
	systemctl --user daemon-reload
	for u in "vpe-rh-guard-run-$n.timer" "vpe-rh-guard-run-$n.service" "vpe-fx-engine-$n.service"; do
		systemctl --user reset-failed "$u" 2>/dev/null
	done
}
