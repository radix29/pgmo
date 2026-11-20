# Plan

pgmo and pgMS share one plan: **[`pgms/docs/PLAN.md`](https://github.com/radix29/pgms/blob/main/docs/PLAN.md)**
(local checkout: `~/go/pgms/docs/PLAN.md`).

pgmo owns phases **P1–P6** (scaffold and text layer, driver and connections,
catalog, object model, scripting, monitoring/EXPLAIN/maintenance) and shares
**P14** (replication and pg_cron), **P15** (testing and release) and **P16**
(post-v1). The upstream baseline (gosmo commit this library is modelled on)
is recorded there and in `CLAUDE.md`.

Do not copy the plan here; update it in pgms.
