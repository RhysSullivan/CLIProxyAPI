#!/usr/bin/env bash
cd /tmp/vpe-dfx && source harness.sh
rm -rf /tmp/vpe-dfx/run-*
fin() { wait_for "$2" "$1 final" "[[ \"\$(awk -F= '\$1==\"phase\"{print \$2}' /tmp/vpe-dfx/run-$1/state/state 2>/dev/null)\" =~ ^(succeeded|rolled-back|recovered|needs-human)\$ ]]"; }
setup f1-success new 18411
setup f2-crash crash 18412
setup f3-badreq badreq 18413
setup f4-kill-swap new 18414; echo pause_at=swap > run-f4-kill-swap/faults
setup f5-kill-checks new 18415; echo pause_at=checks > run-f5-kill-checks/faults
setup f6-rollback-fails badreq 18416; echo restore_fail_once=1 > run-f6-rollback-fails/faults
setup f7-runtime-limit new 18417; echo pause_at=checks > run-f7-runtime-limit/faults; echo runtime_max=25 >> run-f7-runtime-limit/rehearsal.conf
for n in f1-success f2-crash f3-badreq f4-kill-swap f5-kill-checks f6-rollback-fails f7-runtime-limit; do launch $n >/dev/null; done
wait_for 60 "f1 swapped" '[[ "$(awk -F= '"'"'$1=="phase"{print $2}'"'"' /tmp/vpe-dfx/run-f1-success/state/state 2>/dev/null)" == swapped ]]'
echo "CONCURRENT: $(launch f1-success 2>&1) exit=$?"
for x in f4-kill-swap:swap f5-kill-checks:checks; do IFS=: read d p <<<"$x"; wait_for 90 "$d paused" "[[ -f /tmp/vpe-dfx/run-$d/paused-$p ]]"; systemctl --user kill --signal=KILL "$(awk -F= '$1=="deploy_unit"{print $2}' run-$d/state/state)"; echo "KILLED $d at $p"; done
for n in f1-success f2-crash f3-badreq f4-kill-swap f5-kill-checks f6-rollback-fails f7-runtime-limit; do fin $n 200; done
launch f1-success >/dev/null; sleep 3; echo "RERUN: $(tail -1 "$(ls -t run-f1-success/state/logs/deploy-*.log | head -1)")"
sleep 1
for n in f1-success f2-crash f3-badreq f4-kill-swap f5-kill-checks f6-rollback-fails f7-runtime-limit; do
  [[ $n == f1-success ]] && e=new || e=old
  report $n $e | sed -n '1,9p'; echo "result: $(grep RESULT run-$n/state/logs/deploy-2*.log | tail -1 | cut -c1-150)"; echo
done
for n in f1-success f2-crash f3-badreq f4-kill-swap f5-kill-checks f6-rollback-fails f7-runtime-limit; do teardown $n; done
echo "leftover rehearsal units: $(systemctl --user list-units --all --no-legend 'vpe-*' | wc -l); unit files: $(ls ~/.config/systemd/user | grep -c '^vpe-')"
