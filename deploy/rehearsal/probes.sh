# Probes for the two last fixes. Sourced by final-pass.sh (full set) and by
# probes-only.sh (these alone, e.g. against the pre-fix script via VPE_SCRIPT).
# Needs harness.sh and an ALL array plus add() from the caller.

probes_setup() {
	# fix 1: state writes blocked, guard killed during old-engine startup
	# (the old stand-in waits 8 s before listening, so start_old is slow).
	add p1-unwritable-kill-final new 18631 oldslow; echo pause_at=checks >"$B/run-p1-unwritable-kill-final/faults"
	# fix 2: after a rollback, the old stand-in runs the restored config and sees later writes.
	add p2a-config-rollback badreq 18632
	add p2b-config-guard new 18633; echo pause_at=checks >"$B/run-p2b-config-guard/faults"
}

probes_drive() {
	{
		local n=p1-unwritable-kill-final R="$B/run-p1-unwritable-kill-final" i kills=0
		paused $n checks && echo state_write_fails=1 >>"$R/faults" && kill_deploy $n && echo "[p1] state writes now fail, deploy killed"
		# Kill every guard run caught while the old engine is starting (a new
		# "slow start" line in engine.log after the deploy swapped it out), for 60 s.
		local base; base="$(grep -c 'slow start' "$R/live/engine.log")"
		for ((i = 0; i < 120; i++)); do
			if [[ "$(grep -c 'slow start' "$R/live/engine.log")" -gt "$base" &&
				"$(systemctl --user is-active vpe-rh-guard-run-$n.service 2>/dev/null)" =~ ^(active|activating)$ ]]; then
				kill_guard $n KILL; kills=$((kills + 1)); base="$(grep -c 'slow start' "$R/live/engine.log")"
				echo "[p1] killed the guard during old-engine startup ($kills)"
			fi
			sleep 0.5
		done
		echo "$kills" >"$R/kills"
	} &
	{ paused p2b-config-guard checks && kill_deploy p2b-config-guard && echo "[p2b] deploy killed during checks"; } &
}

cfgprobe() { # name port
	local n="$1" port="$2" R="$B/run-$1" got after fails=()
	got="$(curl -s "http://127.0.0.1:$port/standin/config" | cut -d' ' -f1)"
	[[ "$got" == "$(cat "$R/config.sha0")" ]] || fails+=("loaded config ${got:0:12} is not the restored $(cut -c1-12 "$R/config.sha0")")
	# A later write by rename (as the script and editors do), then one in place.
	cp -p "$R/live/merged-config.yaml" "$R/live/.cfg.tmp" && printf '# probe write 1\n' >>"$R/live/.cfg.tmp" && mv -f "$R/live/.cfg.tmp" "$R/live/merged-config.yaml"
	sleep 2; after="$(sha256sum "$R/live/merged-config.yaml" | cut -d' ' -f1)"
	got="$(curl -s "http://127.0.0.1:$port/standin/config" | cut -d' ' -f1)"
	[[ "$got" == "$after" ]] || fails+=("rename write not picked up")
	printf '# probe write 2\n' >>"$R/live/merged-config.yaml"
	sleep 2; after="$(sha256sum "$R/live/merged-config.yaml" | cut -d' ' -f1)"
	got="$(curl -s "http://127.0.0.1:$port/standin/config" | cut -d' ' -f1)"
	[[ "$got" == "$after" ]] || fails+=("in-place write not picked up")
	if ((${#fails[@]})); then echo "FAIL [$n config watch]: ${fails[*]}"; else echo "PASS [$n config watch]: running the restored config; picked up a rename write and an in-place write"; fi
}

probes_assert() {
	# p1 (fix 1): the final action runs once, the timer ends disarmed, the old engine serves.
	local n=p1-unwritable-kill-final R="$B/run-p1-unwritable-kill-final" pid want finals res fails=()
	wait_for 90 "p1 guard timer stopped" "[[ \"\$(systemctl --user is-active vpe-rh-guard-run-$n.timer)\" != active ]]"
	wait_for 30 "p1 old engine up" "curl -sf -o /dev/null http://127.0.0.1:18631/healthz"
	pid="$(systemctl --user show vpe-fx-engine-$n -p MainPID --value)"; want="$(awk -F= '$1=="old_sha"{print $2}' "$R/rehearsal.conf")"
	# Text both the pre-fix and the fixed script log once per final action.
	finals="$(grep -hc 'restore attempts cannot be counted' "$R"/state/logs/deploy-2*.log)"
	res="$(grep -h RESULT "$R"/state/logs/deploy-2*.log | tail -1)"
	[[ "$finals" == 1 ]] || fails+=("final action started $finals times")
	[[ "$(cat "$R/kills")" -ge 1 ]] || fails+=("probe never caught the guard during old-engine startup")
	[[ "$(sha256sum /proc/$pid/exe | cut -d' ' -f1)" == "$want" ]] || fails+=("old not running")
	[[ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18631/healthz)" == 200 ]] || fails+=("healthz not 200")
	[[ "$(systemctl --user is-active vpe-rh-guard-run-$n.timer 2>/dev/null)" != active ]] || fails+=("guard timer active")
	[[ ! -e "$UD/vpe-rh-guard-run-$n.timer" && ! -e "$UD/vpe-rh-guard-run-$n.service" ]] || fails+=("guard unit files left")
	[[ "$res" == *"it is now making its one final action"*"If no later RESULT line follows"* ]] || fails+=("RESULT is not the cut-off notice")
	[[ "$(grep -rl 'STANDIN-SECRET\|rehearsal-fake-key' "$R/state/state" "$R/state/logs" 2>/dev/null | wc -l)" == 0 ]] || fails+=("secret in state/logs")
	[[ -z "$(ls "$R"/state/.h-* "$R"/state/.body.* 2>/dev/null)" ]] || fails+=("temp key/body files left")
	if ((${#fails[@]})); then echo "FAIL [$n]: ${fails[*]}"; else echo "PASS [$n]"; fi
	echo "    final actions=$finals guard runs killed during old start=$(cat "$R/kills") (state not writable)"; echo "    ${res:0:400}"

	assert p2a-config-rollback old rolled-back "" original "rolled back"
	cfgprobe p2a-config-rollback 18632
	assert p2b-config-guard old recovered "" original "guard restored"
	cfgprobe p2b-config-guard 18633
}
