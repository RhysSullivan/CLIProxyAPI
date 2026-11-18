# Rehearsal of deploy-v8.0.77-rhys.1.sh

Throwaway units and files only. Everything lives under `/tmp/vpe-fx`
(`VPE_FX_BASE` changes it). Each scenario gets its own directory
`/tmp/vpe-fx/run-<name>`, its own engine unit `vpe-fx-engine-<name>` (same
`Restart=always`, `RestartSec=3` as the live unit), its own port and its own
guard `vpe-rh-guard-run-<name>`. The script refuses rehearsal mode if any of
them points at `~/.cli-proxy-api`, `vibeproxy-engine` or port 8318.

The engine is a stand-in (`standin/main.go`), built once per behaviour so
each is a distinct ELF with its own sha: `old` and `new` answer every check;
`crash` exits at start; `badreq` answers `/healthz`, `/v1/models` and
`/pool/usage` but returns 502 with a secret-shaped body to the real-request
checks. No request reaches a provider. The stand-in ignores config keys, so
the config scenarios prove the script's flow only; that the real v8.0.64-rhys.2
accepts the edited config was established in an earlier session, not here.

    mkdir -p /tmp/vpe-fx/stub && cp standin/* /tmp/vpe-fx/stub/ && cd /tmp/vpe-fx/stub
    for m in old:v8.0.64-rhys.2:good new:v8.0.77-rhys.1:good crash:v8.0.77-rhys.1:crash badreq:v8.0.77-rhys.1:badreq; do
      IFS=: read n v mode <<<"$m"; CGO_ENABLED=0 go build -trimpath -ldflags "-X main.Version=$v -X main.Mode=$mode" -o bin-$n .; done
    cp ../../harness.sh ../../final-pass.sh /tmp/vpe-fx/   # from deploy/rehearsal
    /tmp/vpe-fx/final-pass.sh

Fault hooks (rehearsal only, from `<dir>/faults`): `pause_at=` a comma list of
`swap`, `checks`, `restore` (top of every restoration), `restore-config`,
`final` (the guard's final action); a paused run waits up to 300 s, until the
harness kills its unit or deletes `<dir>/paused-<point>`. `restore_fail_once=1`
and `restore_fail_always=1` make the restart onto the old binary fail (shared by
restorations and the final action); `state_write_fails=1` makes every state
write fail. `runtime_max=` in `rehearsal.conf` shortens
RuntimeMaxSec.

## Results (2026-11-18, `final-pass.out`): 17 of 17 pass

Each row asserts the running sha, /healthz, phase, `final_check`, config,
persisted `guard_attempts` <= 3, no "attempt 4" in the log, guard timer
inactive, guard unit files gone, no secret in logs or state, no temp key files.

| Scenario | Ends with | Phase |
|---|---|---|
| a1: deploy killed during checks, guard killed in each of 3 restorations (KILL, TERM, KILL) | final action put old back; old serving, config original | needs-human, final_check=old-serving |
| a2: as a1, and the final action killed too | not repeated; candidate still running, log says so | needs-human, final_check=candidate-running |
| b1: config backup corrupted, candidate fails the real-request check | old serving; "CONFIG RECOVERY INCOMPLETE", claude: block named | rolled-back, config_restored=no |
| b2: config backup corrupted, candidate crashes at start | old started and serving; same note | rolled-back, config_restored=no |
| b3: config backup corrupted, deploy killed during checks | guard attempt 1 restores old; same note | recovered, config_restored=no |
| c1: corrupted backup and guard killed in 3 restorations | final action put old back; config not restored, said so | needs-human, final_check=old-serving |
| u1: state writes fail after the deploy is killed | guard cannot count, so it makes its final action and stops: old serving, log says why | (state unwritable), guard gone |
| u2: attempt count unreadable (`guard_attempts=x`) | treated as the limit: final action, old serving, config original | needs-human, final_check=old-serving |
| r1: guard killed at the config step of attempt 1 | attempt 2 finds old running, restores config | recovered, guard_attempts=2 |
| clean success; second deploy while one runs; re-run after success | new serving; refused (exit 4); "already deployed" | succeeded |
| candidate crashes at start | old serving, config original | rolled-back |
| candidate fails the real-request check | old serving, no body logged | rolled-back |
| SIGKILL right after the swap / during the health checks | old serving | recovered (guard) |
| rollback restart fails once | old serving | recovered (guard, attempt 1) |
| RuntimeMaxSec cuts a hung deploy (SIGTERM) | old serving | recovered (guard) |
| restart onto old always fails | 3 attempts + final action fail; candidate still running, log says so | needs-human, final_check=candidate-running |

Not rehearsed: the user manager itself dying mid-deploy. The guard is an
enabled unit-file pair so it should come back with the manager, and the
attempt count lives in the state file, but that was not tested.
