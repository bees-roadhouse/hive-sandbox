package store_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// TestMigrationsLoad runs without a database: it proves the embedded set parses
// and is ordered, so a misnamed file fails the gate anywhere.
func TestMigrationsLoad(t *testing.T) {
	t.Parallel()

	migrations, err := store.Migrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations embedded")
	}
	if migrations[0].Version != "0001" {
		t.Fatalf("first migration is %s, want 0001", migrations[0].Version)
	}
	for _, m := range migrations {
		if len(m.Checksum) != 64 {
			t.Fatalf("migration %s has checksum %q", m.Version, m.Checksum)
		}
		if m.SQL == "" {
			t.Fatalf("migration %s is empty", m.Version)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	s, ctx := testStore(t)

	// testStore already migrated once; a second pass must apply nothing.
	applied, err := store.Migrate(ctx, s.Pool())
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("second migrate applied %v, want nothing", applied)
	}
}

// TestMigrateConcurrent is the reason the advisory lock exists: two daemons
// booting at once must not both run migration one.
func TestMigrateConcurrent(t *testing.T) {
	s, ctx := testStore(t)

	const racers = 6
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = store.Migrate(context.Background(), s.Pool())
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}

	var count int
	if err := s.Pool().QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	migrations, _ := store.Migrations()
	if count != len(migrations) {
		t.Fatalf("schema_migrations has %d rows, want %d", count, len(migrations))
	}
}

// TestEventPartitions checks the partition helper, and that the DEFAULT
// partition catches a month nobody created. An append-only table that rejects
// an insert is an outage, so the default is not optional.
func TestEventPartitions(t *testing.T) {
	s, ctx := testStore(t)

	blocked, err := store.EnsureEventPartitions(ctx, s.Pool(), 2)
	if err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	if len(blocked) != 0 {
		t.Fatalf("months blocked on a fresh schema: %v", blocked)
	}
	// Idempotent, and specifically not racing itself: the helper uses
	// CREATE TABLE IF NOT EXISTS rather than probe-then-create.
	if _, err := store.EnsureEventPartitions(ctx, s.Pool(), 2); err != nil {
		t.Fatalf("ensure partitions twice: %v", err)
	}

	// relnamespace matters: without it this counts every schema in the database
	// that happens to have an events table, so the test could pass on another
	// test's partitions while this code did nothing.
	var parts int
	if err := s.Pool().QueryRow(ctx, `
		SELECT count(*) FROM pg_inherits i
		  JOIN pg_class p ON p.oid = i.inhparent
		  JOIN pg_namespace n ON n.oid = p.relnamespace
		 WHERE p.relname = 'events'
		   AND n.nspname = current_schema()`).Scan(&parts); err != nil {
		t.Fatalf("count partitions: %v", err)
	}
	if parts != 4 { // default + this month + two ahead
		t.Fatalf("events has %d partitions in this schema, want 4", parts)
	}
}

// TestFutureEventCannotWedgeAPartition is Augie's finding 9. One row dated past
// the last partition lands in the default partition and makes that month
// uncreatable forever, while the append-only trigger means the row cannot be
// deleted either.
func TestFutureEventCannotWedgeAPartition(t *testing.T) {
	s, ctx := testStore(t)
	w := newWorld(t)
	alice := w.human("alice")

	_, err := s.Pool().Exec(ctx, `
		INSERT INTO events (created_at, kind, owner_kind, owner_id, author_actor,
		                    principal_kind, principal_id)
		VALUES (now() + interval '3 months', 'probe', 'user', $1, $1, 'user', $1)`, alice)
	if err == nil {
		t.Fatal("an event dated three months out was accepted; it would wedge that month's partition")
	}

	// Small skew is still fine: rejecting it would make the daemon fragile
	// against an ordinary clock difference.
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO events (created_at, kind, owner_kind, owner_id, author_actor,
		                    principal_kind, principal_id)
		VALUES (now() + interval '1 minute', 'probe', 'user', $1, $1, 'user', $1)`, alice); err != nil {
		t.Fatalf("a minute of clock skew was rejected: %v", err)
	}
}

// A blocked month is reported, not fatal. Writes still land in the default
// partition, so failing the boot would turn degraded into down, every restart.
func TestBlockedPartitionIsReportedNotFatal(t *testing.T) {
	s, ctx := testStore(t)
	w := newWorld(t)
	alice := w.human("alice")

	// Reach past the trigger the way only a backfill could: a past month whose
	// partition does not exist yet.
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO events (created_at, kind, owner_kind, owner_id, author_actor,
		                    principal_kind, principal_id)
		VALUES (date_trunc('month', now()) - interval '2 months', 'backfill',
		        'user', $1, $1, 'user', $1)`, alice); err != nil {
		t.Fatalf("insert backfill row: %v", err)
	}

	var name *string
	if err := s.Pool().QueryRow(ctx,
		`SELECT ensure_events_partition((date_trunc('month', now()) - interval '2 months')::date)`,
	).Scan(&name); err != nil {
		t.Fatalf("ensure_events_partition raised instead of reporting: %v", err)
	}
	if name != nil {
		t.Fatalf("the blocked month reported success as %q", *name)
	}

	// And the months that are not blocked still get created.
	blocked, err := store.EnsureEventPartitions(ctx, s.Pool(), 1)
	if err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	if len(blocked) != 0 {
		t.Fatalf("unrelated months blocked: %v", blocked)
	}
}

// TestPartitionBoundsAreUTCRegardlessOfSessionTimezone.
//
// created_at is timestamptz, so a partition bound written as a bare date is
// resolved in the SESSION's TimeZone. Two hosts with different TimeZone
// settings would compute different seams for the same month, and a row landing
// between them goes to the default partition ... which is the one place rows
// can never be pruned from and cannot be deleted, because events is
// append-only.
//
// Found by a test creating twelve months from a client in America/New_York,
// where the fifth month collided with rows the first four had already filed.
//
// The assertion is behavioural rather than textual on purpose: Postgres renders
// a stored bound in whatever zone is asking, so "2027-02-28 19:00:00-05" and
// "2027-03-01 00:00:00+00" are the same instant and a string comparison would
// fail on a correct implementation.
func TestPartitionBoundsAreUTCRegardlessOfSessionTimezone(t *testing.T) {
	s, ctx := testStore(t)
	w := newWorld(t)
	alice := w.human("alice")

	// A distinct past month per zone. Sharing one month would make this test
	// prove nothing: CREATE TABLE IF NOT EXISTS is a no-op once the first
	// subtest has created it, so every later zone would silently inherit
	// bounds computed under the first one. Widely separated so that "the
	// instant before this month" cannot land in a neighbour's partition.
	//
	// Past months, because a trigger refuses future-dated events.
	months := []string{"2024-03", "2024-06", "2024-09"}

	for i, tz := range []string{"UTC", "America/New_York", "Asia/Kolkata"} {
		month := months[i] + "-01"
		t.Run(tz, func(t *testing.T) {
			conn, err := s.Pool().Acquire(ctx)
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			defer conn.Release()
			if _, err := conn.Exec(ctx, "SET TIME ZONE "+quoteLiteral(tz)); err != nil {
				t.Fatalf("set tz: %v", err)
			}

			var name *string
			if err := conn.QueryRow(ctx,
				"SELECT ensure_events_partition($1::date)", month).Scan(&name); err != nil {
				t.Fatalf("ensure: %v", err)
			}
			want := "events_" + strings.ReplaceAll(months[i], "-", "_")
			if name == nil || *name != want {
				t.Fatalf("partition name %v under TimeZone=%s, want %s", name, tz, want)
			}

			// The first instant of the UTC month belongs to that month's
			// partition, and the last instant before it does not. Every session
			// has to agree, whatever zone it is reporting in.
			cases := []struct {
				at   string
				want string
			}{
				{months[i] + "-01 00:00:00+00", want},
				{months[i] + "-01 00:00:00+00", want},
			}
			// The instant one second BEFORE the UTC month start must fall
			// outside, and there is no neighbouring partition to catch it.
			var before string
			if err := conn.QueryRow(ctx,
				`SELECT to_char(($1::timestamptz - interval '1 second') AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') || '+00'`,
				months[i]+"-01 00:00:00+00").Scan(&before); err != nil {
				t.Fatalf("compute boundary: %v", err)
			}
			cases[1] = struct {
				at   string
				want string
			}{before, "events_default"}
			for j, c := range cases {
				var landed string
				if err := conn.QueryRow(ctx, `
					INSERT INTO events (created_at, kind, owner_kind, owner_id, author_actor,
					                    principal_kind, principal_id)
					VALUES ($1::timestamptz, $2, 'user', $3, $3, 'user', $3)
					RETURNING tableoid::regclass::text`,
					c.at, fmt.Sprintf("tz.%d.%d", i, j), alice).Scan(&landed); err != nil {
					t.Fatalf("insert at %s: %v", c.at, err)
				}
				if landed != c.want {
					t.Fatalf("under TimeZone=%s a row at %s landed in %q, want %q; "+
						"the month seam moved with the session zone",
						tz, c.at, landed, c.want)
				}
			}
		})
	}
}

// quoteLiteral is enough for a timezone name; SET does not take a parameter.
func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
