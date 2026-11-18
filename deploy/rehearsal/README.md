# Rehearsal of deploy-v8.0.77-rhys.1.sh

Throwaway units and files only. Each scenario gets its own directory
`/tmp/vpe-dfx/run-<name>`, its own engine unit `vpe-rh-engine-<name>` (same
`Restart=always`, `RestartSec=3` as the live unit), its own port and its own
guard `vpe-rh-guard-run-<name>`. The script refuses rehearsal mode if any of
them points at `~/.cli-proxy-api`, `vibeproxy-engine` or port 8318.

The engine is a stand-in (`standin/main.go`), built once per behaviour so
each is a distinct ELF with its own sha: `old` and `new` answer every check;
`crash` exits at start; `badreq` answers `/healthz`, `/v1/models` and
`/pool/usage` but returns 502 with a secret-shaped body to the real-request
checks. No request reaches a provider.

    mkdir -p /tmp/vpe-dfx/stub && cp standin/* /tmp/vpe-dfx/stub/ && cd /tmp/vpe-dfx/stub
    for m in old:v8.0.64-rhys.2:good new:v8.0.77-rhys.1:good crash:v8.0.77-rhys.1:crash badreq:v8.0.77-rhys.1:badreq; do
      IFS=: read n v mode <<<"$m"; CGO_ENABLED=0 go build -trimpath -ldflags "-X main.Version=$v -X main.Mode=$mode" -o bin-$n .; done
    cp ../../harness.sh ../../final-pass.sh /tmp/vpe-dfx/   # from deploy/rehearsal
    /tmp/vpe-dfx/final-pass.sh

Fault hooks (rehearsal only, from `<dir>/faults`): `pause_at=swap|checks`,
`restore_fail_once=1`, `restore_fail_always=1`; `runtime_max=` in
`rehearsal.conf` shortens RuntimeMaxSec.

## Results (2026-11-18, `final-pass.out`)

| Scenario | Ends with | Phase |
|---|---|---|
| clean success | new serving, config has the fallback key | succeeded |
| candidate crashes at start | old serving, config original | rolled-back (deploy shell) |
| candidate fails the real-request check | old serving, no body logged | rolled-back (deploy shell) |
| SIGKILL right after the swap | old serving, 11 s after kill | recovered (guard) |
| SIGKILL during the health checks | old serving, 11 s after kill | recovered (guard) |
| rollback restart fails once | old serving | recovered (guard, attempt 1) |
| second deploy while one runs | refused, lock held, exit 4 | (first one succeeded) |
| re-run after success | "already deployed; nothing changed" | succeeded |
| RuntimeMaxSec cuts a hung deploy (SIGTERM) | old serving | recovered (guard) |
| restore always fails (separate run) | candidate still running, old installed | needs-human after 3 attempts |

Not rehearsed: the user manager itself dying mid-deploy. The guard is an
enabled unit-file pair so it should come back with the manager, but that was
not tested.
