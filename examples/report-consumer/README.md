# Import a verified report into your SaaS database

AgentMoor publishes a versioned file. Your application decides whether the file
is a valid business result. This example shows that boundary using Python 3
(standard library only) and a local SQLite database.

## Try it without a model or cluster

From the repository root:

```sh
make test-report-consumer
```

The tests start a temporary loopback HTTP server using AgentMoor-style receipts.
The consumer runs as a separate process and writes a temporary business
database. The tests do **not** start AgentMoor, a native model CLI, or Kubernetes.

## Import from a running AgentMoor service

Prepare a local JSON file containing independently calculated expected totals,
such as `{"totals_cents":{"east":98,"west":31}}`. For a completed task that
publishes this report format, inject a short-lived, scoped AgentMoor read token
as the `AGENTMOOR_READ_TOKEN` environment variable using your application's
existing secret mechanism. Then run:

```sh
python3 examples/report-consumer/ingest.py \
  --endpoint https://agentmoor.example.com \
  --scope example/reports --task report-job \
  --expected expected.json --db reports.db
```

For a completed follow-up whose artifact has the **same JSON schema**, add
`--turn <turn-uuid>` and supply independently expected values for that turn.
The current text-mode follow-up publishes `reply.txt`; ordinary free-form
replies do not meet this example's schema. This importer does not change
AgentMoor's Worker, run tools, or republish modified files from a new Pod.

The importer reads only a successful task or turn and selects the report named
by `spec.output` from its `out/` delivery directory. Other delivered files are
not imported. It downloads the exact artifact version over the same origin,
checks SHA-256, validates the schema
and rejects totals that do not match the independent expectation. It then
stores the report by endpoint, application scope, task, turn, artifact and
version. Exact replays return `already-imported`. A different receipt for the
same identity is rejected and does not replace the stored report. A follow-up
has a separate artifact identity and can have a version number of 1 without
overwriting the first result.

If the business database write fails, the command exits with an error and the
transaction rolls back. Run the same command again after fixing the database;
it downloads the pinned version again and imports it once. No Agent task is
submitted or executed by this retry. `make test-worker-file-report` injects a
real SQLite insert failure after two independent Workers have delivered files.

In production, derive the scope from your trusted tenant/project binding for
the token; this example's `--scope` is a local database label, not an
authorization control. Keep the database in a directory controlled by the SaaS
operator. The receipt is trusted to be from your authenticated AgentMoor
service; the digest validates the downloaded bytes against that receipt.
