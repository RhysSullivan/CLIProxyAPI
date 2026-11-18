#!/usr/bin/env bash
# Run only the fix-1/fix-2 probes. VPE_SCRIPT=<path> runs them against another
# copy of the deploy script (e.g. the pre-fix one, to show the probe catches it).
cd "${VPE_FX_BASE:-/tmp/vpe-fx}" && source ./harness.sh && source ./probes.sh
rm -rf "$B"/run-p*
ALL=(); add() { ALL+=("$1"); setup "$@"; }
probes_setup
for n in "${ALL[@]}"; do launch "$n" >/dev/null; done
probes_drive
wait
for n in p2a-config-rollback p2b-config-guard; do fin "$n" 300; done
sleep 2
probes_assert
for n in "${ALL[@]}"; do teardown "$n"; done
echo "leftover rehearsal units: $(systemctl --user list-units --all --no-legend 'vpe-fx-*' 'vpe-rh-*' | grep -vc 'not-found')"
echo "leftover unit files: $(ls ~/.config/systemd/user ~/.config/systemd/user/timers.target.wants | grep -c '^vpe-')"
