#!/usr/bin/env bash
# Rehearsal harness for deploy-v8.0.77-rhys.1.sh. Throwaway units and files only.
set -uo pipefail
SCRIPT=/home/rhys/agent-workspace/.rifts/vibeproxy-engine/rhys-v8077/deploy/deploy-v8.0.77-rhys.1.sh
STUB=/tmp/vpe-dfx/stub
UD="$HOME/.config/systemd/user"
OLD_SHA="$(sha256sum $STUB/bin-old | cut -d' ' -f1)"
NEW_SHA="$(sha256sum $STUB/bin-new | cut -d' ' -f1)"

say() { printf '\n### %s\n' "$*"; }

setup() { # name candidate port
	local n="$1" cand="$2" port="$3" R="/tmp/vpe-dfx/run-$1"
	mkdir -p "$R/live" "$R/cand" "$R/state"; chmod 700 "$R"
	cp "$STUB/bin-old" "$R/live/cli-proxy-api-plus"
	cp "$STUB/bin-$cand" "$R/cand/cli-proxy-api-plus.v8.0.77-rhys.1"
	printf 'api-keys:\n  - "rehearsal-fake-key"\nhost: 127.0.0.1\nport: %s\ndebug: false\n' "$port" >"$R/live/merged-config.yaml"
	chmod 600 "$R/live/merged-config.yaml"
	sha256sum "$R/live/merged-config.yaml" | cut -d' ' -f1 >"$R/config.sha0"
	cat >"$R/rehearsal.conf" <<-EOF
	unit=vpe-rh-engine-$n
	port=$port
	old_version=v8.0.64-rhys.2
	old_sha=$OLD_SHA
	new_version=v8.0.77-rhys.1
	new_sha=$(sha256sum "$STUB/bin-$cand" | cut -d' ' -f1)
	new_bin=$R/cand/cli-proxy-api-plus.v8.0.77-rhys.1
	EOF
	cat >"$UD/vpe-rh-engine-$n.service" <<-EOF
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
	systemctl --user start "vpe-rh-engine-$n"
	timeout 20 bash -c "until curl -sf -o /dev/null http://127.0.0.1:$port/healthz; do sleep 0.5; done" || echo "SETUP FAILED"
}

wait_for() { # seconds description command...
	local t="$1" d="$2"; shift 2
	timeout "$t" bash -c "until $*; do sleep 1; done" || { echo "TIMEOUT waiting for $d"; return 1; }
}
phase() { awk -F= '$1=="phase"{print $2}' "/tmp/vpe-dfx/run-$1/state/state" 2>/dev/null; }
final() { [[ "$(phase "$1")" =~ ^(succeeded|rolled-back|recovered|needs-human|nothing-changed)$ ]]; }

report() { # name expect(old|new)
	local n="$1" R="/tmp/vpe-dfx/run-$1" pid exe port want
	port="$(awk -F= '$1=="port"{print $2}' "$R/rehearsal.conf")"
	pid="$(systemctl --user show "vpe-rh-engine-$n" -p MainPID --value)"
	exe="$(sha256sum "/proc/$pid/exe" 2>/dev/null | cut -d' ' -f1)"
	[[ "$2" == old ]] && want="$OLD_SHA" || want="$(awk -F= '$1=="new_sha"{print $2}' "$R/rehearsal.conf")"
	echo "--- outcome [$n]"
	echo "phase=$(phase "$n")"
	echo "serving: pid=$pid exe=${exe:0:12} expected-$2=${want:0:12} match=$([[ "$exe" == "$want" ]] && echo yes || echo NO)"
	echo "healthz=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$port/healthz")"
	echo "installed=$(sha256sum "$R/live/cli-proxy-api-plus" | cut -c1-12)"
	echo "config=$( [[ "$(sha256sum "$R/live/merged-config.yaml" | cut -d' ' -f1)" == "$(cat "$R/config.sha0")" ]] && echo original || echo "edited ($(grep -c server-side-fallback "$R/live/merged-config.yaml") fallback line)")"
	echo "guard units left: $(ls "$UD" | grep -c "vpe-rh-guard-run-$n") ; guard timer: $(systemctl --user is-active "vpe-rh-guard-run-$n.timer" 2>/dev/null)"
	echo "secret in logs: $(grep -rl 'STANDIN-SECRET\|rehearsal-fake-key' "$R/state/logs" 2>/dev/null | wc -l) files"
	echo "--- log"
	cat "$R"/state/logs/deploy-*.log
}

launch() { VPE_REHEARSAL_DIR="/tmp/vpe-dfx/run-$1" bash "$SCRIPT" launch; }

teardown() {
	local n="$1"
	systemctl --user stop "vpe-rh-engine-$n" "vpe-rh-guard-run-$n.timer" 2>/dev/null
	rm -f "$UD/vpe-rh-engine-$n.service" "$UD/vpe-rh-guard-run-$n.service" "$UD/vpe-rh-guard-run-$n.timer"
	systemctl --user daemon-reload
	systemctl --user reset-failed "vpe-rh-engine-$n" "vpe-rh-guard-run-$n.service" 2>/dev/null
}
