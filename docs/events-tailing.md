# Tailing the events table

Read this before writing anything that consumes `events`. Migration one made a
choice that changes the shape of the cursor, and getting it wrong produces a bug
that only shows up under load or after a few months of partitions.

## The rules that do not change

From D4 (Hallie's findings), unchanged by partitioning:

1. **The events table is the transport. NOTIFY is a wakeup bell carrying an id.**
   Every consumer must stay correct if every notification is dropped.
2. **Never tail with a naive `WHERE id > last`.** `bigserial` ids are assigned
   before commit, so an id assigned early and committed late is permanently
   skipped. Use an overlap window and dedupe by id.
3. **Backstop poll every 5 to 30 seconds** regardless of connection health. That
   is what turns a missed notification into a latency event rather than a
   correctness event.

## What partitioning changed

`events` is `PARTITION BY RANGE (created_at)`, monthly, because retention is
"keep" and growth has to stay an operational non-event. Two consequences:

### The cursor is `(created_at, id)`, not `id`

A partitioned table has no global index on `id` alone. There is a *local* index
on each partition, so `WHERE id > $1 ORDER BY id` still works, but it probes
every partition on every poll and that cost grows forever. After two years that
is twenty-four index probes per tailer per poll, for one row.

Carry both halves of the cursor and bound the time range:

```sql
SELECT id, created_at, kind, body
  FROM events
 WHERE created_at >= $1::timestamptz - interval '5 seconds'   -- overlap window
   AND (created_at, id) > ($1, $2)
 ORDER BY created_at, id
 LIMIT 500;
```

The time bound is what prunes to one or two partitions. The overlap window is
rule 2 above, and it is still required: **partitioning does not fix the id gap,
and it does not fix the timestamp gap either.** `created_at` defaults to
`clock_timestamp()` rather than `now()` precisely so a long transaction does not
file rows under its start time, but a row is still only *visible* at commit, so
a consumer that advanced past a timestamp can miss a row bearing it.

Dedupe by `id` after the overlap re-read. Handlers must be idempotent anyway.

### `Last-Event-ID` has to carry both

SSE reconnect (D4.13) sends back whatever was in the last `id:` field. Emit the
composite, not the bare id:

```
id: 1736899200123456-4711
```

that is `<created_at as microseconds since epoch>-<id>`. A client never parses
it; it hands it back verbatim and the host splits it. A host that receives a
bare integer (an old client, or a hand-written curl) should fall back to
resolving the timestamp with one lookup:

```sql
SELECT created_at FROM events WHERE id = $1;
```

That is one all-partition probe per connect, which is fine. One per poll is not.

**Replay filters with CURRENT permissions**, never permissions as of the event
(D4.13). Use `access_reason()` like any other read; a revoked grant must not be
replayed around.

## Partitions are created ahead of time

`store.EnsureEventPartitions(ctx, db, monthsAhead)` creates the current month
plus N ahead. Call it at boot and on a daily timer.

There is also a `DEFAULT` partition, because an append-only table that rejects
an insert because nobody made next month's partition is an outage. It should
stay empty. **Rows that land in the default partition cannot be pruned by
dropping a partition later**, and attaching a partition covering their range
requires moving them first, so treat a non-empty default partition as an alert
rather than as a working state:

```sql
SELECT count(*) FROM events_default;
```

## `(origin, origin_id)` uniqueness lives in a side table

D4.12 asks for a unique constraint on `(origin, origin_id)` from migration one,
so cross-hive bridging can dedupe later. A `UNIQUE` constraint on a partitioned
table **must include the partition key**, which would make it unique per month
and useless for that purpose.

So `event_origins (origin, origin_id) PRIMARY KEY` carries the real constraint,
written by an `AFTER INSERT` trigger on `events`, and only for rows where
`origin_id IS NOT NULL`. Locally produced events leave it NULL and cost nothing.
A duplicate bridged event raises a unique violation on insert, which is the
behaviour D4.12 wanted.

Do not add a `UNIQUE (created_at, origin, origin_id)` to `events` believing it
does the same thing. It does not.
