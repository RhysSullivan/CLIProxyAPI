#!/usr/bin/env bash
# Full rehearsal pass. Every scenario runs at once, each with its own unit,
# port and guard; each ends with PASS/FAIL lines from assert().
cd "${VPE_FX_BASE:-/tmp/vpe-fx}" && source ./harness.sh
rm -rf "$B"/run-*

# assert name serving(old|new|candidate) phase final_check config(original|edited) [result-substring]
assert() {
	local n="$1" R="$B/run-$1" want_run="$2" want_phase="$3" want_check="$4" want_cfg="$5" want_res="${6:-}"
	local pid exe want port fails=() att max cfg res
	port="$(awk -F= '$1=="port"{print $2}' "$R/rehearsal.conf")"
	pid="$(systemctl --user show "vpe-fx-engine-$n" -p MainPID --value)"
	exe="$(sha256sum "/proc/$pid/exe" 2>/dev/null | cut -d' ' -f1)"
	case "$want_run" in old) want="$OLD_SHA" ;; *) want="$(awk -F= '$1=="new_sha"{print $2}' "$R/rehearsal.conf")" ;; esac
	[[ "$exe" == "$want" ]] || fails+=("running ${exe:0:12} not $want_run")
	if [[ "$want_run" != candidate ]]; then
		[[ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$port/healthz")" == 200 ]] || fails+=("healthz not 200")
	fi
	[[ "$(phase "$n")" == "$want_phase" ]] || fails+=("phase $(phase "$n") not $want_phase")
	[[ "$(st "$n" final_check)" == "$want_check" ]] || fails+=("final_check '$(st "$n" final_check)' not '$want_check'")
	[[ "$(sha256sum "$R/live/merged-config.yaml" | cut -d' ' -f1)" == "$(cat "$R/config.sha0")" ]] && cfg=original || cfg=edited
	[[ "$cfg" == "$want_cfg" ]] || fails+=("config $cfg not $want_cfg")
	att="$(st "$n" guard_attempts)"; [[ ! "$att" =~ ^[0-9]+$ || "$att" -le 3 ]] || fails+=("persisted guard_attempts=$att > 3")
	max="$(grep -ho 'restore attempt [0-9]*/' "$R"/state/logs/deploy-*.log | tr -dc '0-9\n' | sort -n | tail -1)"
	[[ -z "$max" || "$max" -le 3 ]] || fails+=("log reached attempt $max")
	[[ "$(systemctl --user is-active "vpe-rh-guard-run-$n.timer" 2>/dev/null)" != active ]] || fails+=("guard timer still active")
	[[ ! -e "$UD/vpe-rh-guard-run-$n.timer" && ! -e "$UD/vpe-rh-guard-run-$n.service" ]] || fails+=("guard unit files left")
	[[ "$(grep -rl 'STANDIN-SECRET\|rehearsal-fake-key' "$R/state/state" "$R/state/logs" 2>/dev/null | wc -l)" == 0 ]] || fails+=("secret in state/logs")
	[[ -z "$(ls "$R"/state/.h-* "$R"/state/.body.* 2>/dev/null)" ]] || fails+=("temp key/body files left")
	res="$(grep -h 'RESULT' "$R"/state/logs/deploy-2*.log | tail -1)"
	[[ -z "$want_res" || "$res" == *"$want_res"* ]] || fails+=("RESULT lacks '$want_res'")
	[[ "$(tail -1 "$(ls -t "$R"/state/logs/deploy-2*.log | tail -1)")" == *RESULT* ]] || fails+=("first deploy log does not end with RESULT")
	if ((${#fails[@]})); then echo "FAIL [$n]: ${fails[*]}"; else echo "PASS [$n]"; fi
	echo "    phase=$(phase "$n") attempts=${att:-0} final_action=$(st "$n" final_action) final_check=$(st "$n" final_check) config_restored=$(st "$n" config_restored) running=${exe:0:12} config=$cfg"
	echo "    ${res:0:400}"
}

# Kill the guard each time it pauses in a restoration, alternating SIGKILL and SIGTERM.
kill_guard_restores() { # name times
	local i
	for ((i = 1; i <= $2; i++)); do
		paused "$1" restore 60 || return 1
		echo "[$1] guard paused in restore attempt $(st "$1" guard_attempts); killing it ($( ((i % 2)) && echo KILL || echo TERM))"
		((i % 2)) && kill_guard "$1" KILL || kill_guard "$1" TERM
	done
}

ALL=()
add() { ALL+=("$1"); setup "$@"; }
# --- new: (a) guard killed during restorations
add a1-kill-restore new 18621; echo pause_at=checks,restore >"$B/run-a1-kill-restore/faults"
add a2-kill-final new 18622; echo pause_at=checks,restore,final >"$B/run-a2-kill-final/faults"
# --- new: (b) corrupted config backup
add b1-corrupt-badreq badreq 18623; echo pause_at=swap >"$B/run-b1-corrupt-badreq/faults"
add b2-corrupt-crash crash 18624; echo pause_at=swap >"$B/run-b2-corrupt-crash/faults"
add b3-corrupt-guard new 18625; echo pause_at=checks >"$B/run-b3-corrupt-guard/faults"
# --- new: (c) both
add c1-both new 18626; echo pause_at=checks,restore >"$B/run-c1-both/faults"
# --- paths around the attempt count
add u1-unwritable new 18627; echo pause_at=checks >"$B/run-u1-unwritable/faults"
add u2-count-garbage new 18628; echo pause_at=checks >"$B/run-u2-count-garbage/faults"
add r1-kill-config-step new 18629; echo pause_at=checks,restore-config >"$B/run-r1-kill-config-step/faults"
# --- earlier scenarios
add f1-success new 18611
add f2-crash crash 18612
add f3-badreq badreq 18613
add f4-kill-swap new 18614; echo pause_at=swap >"$B/run-f4-kill-swap/faults"
add f5-kill-checks new 18615; echo pause_at=checks >"$B/run-f5-kill-checks/faults"
add f6-rollback-fails badreq 18616; echo restore_fail_once=1 >"$B/run-f6-rollback-fails/faults"
add f7-runtime-limit new 18617; echo pause_at=checks >"$B/run-f7-runtime-limit/faults"; echo runtime_max=25 >>"$B/run-f7-runtime-limit/rehearsal.conf"
add f8-restore-never badreq 18618; echo restore_fail_always=1 >"$B/run-f8-restore-never/faults"

for n in "${ALL[@]}"; do launch "$n" >/dev/null; done
wait_for 60 "f1 swapped" "[[ \"\$(awk -F= '\$1==\"phase\"{print \$2}' $B/run-f1-success/state/state 2>/dev/null)\" == swapped ]]"
echo "CONCURRENT: $(launch f1-success 2>&1) exit=$?"

{ paused a1-kill-restore checks && kill_deploy a1-kill-restore && kill_guard_restores a1-kill-restore 3; } &
{ paused a2-kill-final checks && kill_deploy a2-kill-final && kill_guard_restores a2-kill-final 3 &&
	paused a2-kill-final final 60 && echo "[a2] guard paused in its final action; killing it" && kill_guard a2-kill-final KILL; } &
for n in b1-corrupt-badreq b2-corrupt-crash; do { paused $n swap && corrupt_backup $n && resume $n swap && echo "[$n] backup corrupted"; } & done
{ paused b3-corrupt-guard checks && corrupt_backup b3-corrupt-guard && kill_deploy b3-corrupt-guard && echo "[b3] backup corrupted, deploy killed"; } &
{ paused c1-both checks && corrupt_backup c1-both && kill_deploy c1-both && echo "[c1] backup corrupted, deploy killed" && kill_guard_restores c1-both 3; } &
{ paused u1-unwritable checks && echo state_write_fails=1 >>"$B/run-u1-unwritable/faults" && kill_deploy u1-unwritable && echo "[u1] state writes now fail, deploy killed"; } &
{ paused u2-count-garbage checks && sed -i 's/^guard_attempts=.*/guard_attempts=x/' "$B/run-u2-count-garbage/state/state" && kill_deploy u2-count-garbage && echo "[u2] guard_attempts=x, deploy killed"; } &
{ paused r1-kill-config-step checks && kill_deploy r1-kill-config-step && paused r1-kill-config-step restore-config 60 &&
	echo "[r1] guard paused at the config step of attempt $(st r1-kill-config-step guard_attempts); killing it, clearing faults" &&
	kill_guard r1-kill-config-step KILL && : >"$B/run-r1-kill-config-step/faults"; } &
for x in f4-kill-swap:swap f5-kill-checks:checks; do IFS=: read -r d p <<<"$x"; { paused "$d" "$p" && kill_deploy "$d" && echo "[$d] deploy killed at $p"; } & done
wait

for n in "${ALL[@]}"; do [[ $n == u1-unwritable ]] || fin "$n" 300; done
wait_for 120 "u1 guard timer stopped" "[[ \"\$(systemctl --user is-active vpe-rh-guard-run-u1-unwritable.timer)\" != active ]]"
launch f1-success >/dev/null; sleep 3; echo "RERUN: $(tail -1 "$(ls -t "$B"/run-f1-success/state/logs/deploy-*.log | head -1)" | cut -c11-)"
sleep 2
echo
assert a1-kill-restore old needs-human old-serving original "SERVICE IS UP ON v8.0.64-rhys.2"
assert a2-kill-final candidate needs-human candidate-running edited "CANDIDATE v8.0.77-rhys.1 IS STILL RUNNING"
assert b1-corrupt-badreq old rolled-back "" edited "CONFIG RECOVERY INCOMPLETE"
assert b2-corrupt-crash old rolled-back "" edited "CONFIG RECOVERY INCOMPLETE"
assert b3-corrupt-guard old recovered "" edited "CONFIG RECOVERY INCOMPLETE"
assert c1-both old needs-human old-serving edited "CONFIG NOT RESTORED"
assert u2-count-garbage old needs-human old-serving original "found its attempt count unreadable"
assert r1-kill-config-step old recovered "" original "guard restored"
# u1: state writes fail, so no phase is recorded; check the rest by hand.
u1R="$B/run-u1-unwritable"; u1pid="$(systemctl --user show vpe-fx-engine-u1-unwritable -p MainPID --value)"
u1res="$(grep -h RESULT "$u1R"/state/logs/deploy-2*.log | tail -1)"
if [[ "$(sha256sum /proc/$u1pid/exe | cut -d' ' -f1)" == "$OLD_SHA" && "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18627/healthz)" == 200 &&
	"$u1res" == *"cannot write"*"SERVICE IS UP ON"* && "$(systemctl --user is-active vpe-rh-guard-run-u1-unwritable.timer)" != active &&
	! -e "$UD/vpe-rh-guard-run-u1-unwritable.timer" && "$(grep -c 'restore attempt [0-9]' "$u1R"/state/logs/deploy-2*.log)" == 0 ]]; then
	echo "PASS [u1-unwritable]"; else echo "FAIL [u1-unwritable]"; fi
echo "    phase=$(phase u1-unwritable) (unchanged: state not writable)"; echo "    ${u1res:0:400}"
assert f1-success new succeeded "" edited "already deployed"
assert f2-crash old rolled-back "" original "rolled back"
assert f3-badreq old rolled-back "" original "rolled back"
assert f4-kill-swap old recovered "" original "guard restored"
assert f5-kill-checks old recovered "" original "guard restored"
assert f6-rollback-fails old recovered "" original "guard restored"
assert f7-runtime-limit old recovered "" original "guard restored"
assert f8-restore-never candidate needs-human candidate-running edited "CANDIDATE v8.0.77-rhys.1 IS STILL RUNNING"
echo
for n in a2-kill-final c1-both r1-kill-config-step u1-unwritable; do echo "--- guard log tail [$n]"; grep -h 'guard\|final\|RESULT\|CONFIG' "$B/run-$n"/state/logs/deploy-2*.log | cut -c1-220 | tail -9; done

for n in "${ALL[@]}"; do teardown "$n"; done
echo
echo "leftover rehearsal units: $(systemctl --user list-units --all --no-legend 'vpe-fx-*' 'vpe-rh-*' | grep -vc 'not-found')"
echo "leftover unit files: $(ls ~/.config/systemd/user ~/.config/systemd/user/timers.target.wants | grep -c '^vpe-')"
