package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Cursor is a position in the events stream.
//
// It is a PAIR, not an id, and that is forced by the schema: events is
// partitioned by created_at, so there is no global index on id alone and an
// id-only tail probes every partition on every poll. The timestamp is what
// prunes partitions; the id is what breaks ties and makes the position exact.
//
// See docs/events-tailing.md.
type Cursor struct {
	At time.Time
	ID int64
}

// Zero reports whether the cursor names no position.
func (c Cursor) Zero() bool { return c.ID == 0 && c.At.IsZero() }

// Before reports whether c sorts before other under (created_at, id).
func (c Cursor) Before(other Cursor) bool {
	if !c.At.Equal(other.At) {
		return c.At.Before(other.At)
	}
	return c.ID < other.ID
}

// String encodes the cursor for an SSE `id:` field: microseconds since epoch,
// a hyphen, then the row id. Clients treat it as opaque and hand it back.
func (c Cursor) String() string {
	if c.Zero() {
		return ""
	}
	return strconv.FormatInt(c.At.UnixMicro(), 10) + "-" + strconv.FormatInt(c.ID, 10)
}

// ParseCursor decodes what a client sent back in Last-Event-ID.
//
// A bare integer is accepted and yields an id with no timestamp, because an
// older client (or a hand-written curl) may hold a pre-partitioning cursor. The
// caller resolves the timestamp with one lookup; see ResolveCursor. That is one
// all-partition probe per connect, which is fine. One per poll is not.
func ParseCursor(s string) (Cursor, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Cursor{}, nil
	}
	micros, id, found := strings.Cut(s, "-")
	if !found {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return Cursor{}, fmt.Errorf("cursor %q: %w", s, err)
		}
		return Cursor{ID: n}, nil
	}
	m, err := strconv.ParseInt(micros, 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("cursor %q: bad timestamp: %w", s, err)
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("cursor %q: bad id: %w", s, err)
	}
	return Cursor{At: time.UnixMicro(m).UTC(), ID: n}, nil
}

// ResolveCursor fills in a missing timestamp for a bare-id cursor. Call it once
// per connection, never per poll.
func ResolveCursor(ctx context.Context, db DB, c Cursor) (Cursor, error) {
	if c.ID == 0 || !c.At.IsZero() {
		return c, nil
	}
	var at time.Time
	err := db.QueryRow(ctx, "SELECT created_at FROM events WHERE id = $1", c.ID).Scan(&at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The id is gone or was never ours. Treat it as "start from the
		// beginning of what we still keep" rather than guessing.
		return Cursor{}, nil
	case err != nil:
		return Cursor{}, fmt.Errorf("resolve cursor %d: %w", c.ID, err)
	}
	c.At = at
	return c, nil
}

// Event is one row of the append-only log. It is the transport of record: a
// NOTIFY carrying its id is a wakeup bell, and every consumer stays correct if
// every notification is dropped.
type Event struct {
	ID        int64
	CreatedAt time.Time
	Kind      string

	// Subject is what the event is about, in the shape access_reason() takes,
	// so a replay filters through the same predicate as a live read.
	Subject Subject
	Owner   Owner

	AuthorActor   uuid.UUID
	PrincipalKind PrincipalKind
	PrincipalID   uuid.UUID

	Body       json.RawMessage
	Trust      string
	CauseDepth int
	RunID      *uuid.UUID

	Origin   string
	OriginID *string
}

// Cursor is the event's own position.
func (e Event) Cursor() Cursor { return Cursor{At: e.CreatedAt, ID: e.ID} }

const eventColumns = `id, created_at, kind, subject_kind, subject_id, subject_name,
	owner_kind, owner_id, author_actor, principal_kind, principal_id,
	body, trust, cause_depth, run_id, origin, origin_id`

func scanEvents(rows pgx.Rows) ([]Event, error) {
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var (
			e           Event
			subjectKind *string
			subjectID   *uuid.UUID
			subjectName *string
			ownerKind   string
			principal   string
		)
		if err := rows.Scan(
			&e.ID, &e.CreatedAt, &e.Kind, &subjectKind, &subjectID, &subjectName,
			&ownerKind, &e.Owner.ID, &e.AuthorActor, &principal, &e.PrincipalID,
			&e.Body, &e.Trust, &e.CauseDepth, &e.RunID, &e.Origin, &e.OriginID,
		); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.Owner.Kind = PrincipalKind(ownerKind)
		e.PrincipalKind = PrincipalKind(principal)
		if subjectKind != nil {
			e.Subject.Kind = SubjectKind(*subjectKind)
		}
		if subjectID != nil {
			e.Subject.ID = *subjectID
		}
		if subjectName != nil {
			e.Subject.Name = *subjectName
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// NotifyChannel is the one coarse channel (D4.9). Visibility is filtered in the
// host after receipt, not by splitting channels: authorization is domain logic
// and belongs with domain logic.
const NotifyChannel = "hive_events"

// AppendEvents inserts events and issues exactly ONE notification for the whole
// call, carrying the highest cursor written.
//
// One notify per unit of work is not a micro-optimisation: NOTIFY takes a heavy
// lock at commit and serialises commits, so a per-row notify would cost the
// whole write path, not just the notification path (D4.11, D4.17).
//
// Pass a transaction when the events have to land with other writes. A journal
// entry, its mention row and its read grant are one transaction by rule (D13.2),
// and the event belongs in it.
func AppendEvents(ctx context.Context, db DB, events ...*Event) error {
	if len(events) == 0 {
		return nil
	}

	var highest Cursor
	for _, e := range events {
		if e.Origin == "" {
			e.Origin = "local"
		}
		if e.Trust == "" {
			e.Trust = "trusted"
		}
		if len(e.Body) == 0 {
			e.Body = json.RawMessage(`{}`)
		}
		var (
			subjectKind *string
			subjectID   *uuid.UUID
			subjectName *string
		)
		if e.Subject.Kind != "" {
			k := string(e.Subject.Kind)
			subjectKind = &k
			id := e.Subject.ID
			subjectID = &id
			if e.Subject.Name != "" {
				n := e.Subject.Name
				subjectName = &n
			}
		}

		if err := db.QueryRow(ctx, `
			INSERT INTO events (kind, subject_kind, subject_id, subject_name,
			                    owner_kind, owner_id, author_actor,
			                    principal_kind, principal_id,
			                    body, trust, cause_depth, run_id, origin, origin_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			RETURNING id, created_at`,
			e.Kind, subjectKind, subjectID, subjectName,
			string(e.Owner.Kind), e.Owner.ID, e.AuthorActor,
			string(e.PrincipalKind), e.PrincipalID,
			e.Body, e.Trust, e.CauseDepth, e.RunID, e.Origin, e.OriginID,
		).Scan(&e.ID, &e.CreatedAt); err != nil {
			return fmt.Errorf("append event %q: %w", e.Kind, err)
		}
		if highest.Before(e.Cursor()) {
			highest = e.Cursor()
		}
	}

	// The payload is a hint and nothing more. A consumer that never sees it
	// still catches up on its next poll.
	if _, err := db.Exec(ctx, "SELECT pg_notify($1, $2)", NotifyChannel, highest.String()); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	return nil
}

// Tail reads forward from a cursor: the host's view of the log, unfiltered.
//
// Strictly after the cursor, so a full batch always makes progress. An earlier
// version took a bare timestamp derived from the newest row DELIVERED, which
// meant a burst larger than one batch produced a query that returned the same
// oldest rows forever: every row already seen, nothing fresh, the timestamp
// never moving, the batch always full. Two round trips per iteration, delivering
// nothing, until the process was restarted.
//
// The cursor's timestamp is also the lower bound, which is what prunes
// partitions. Rows that commit late with a position BELOW the cursor are not
// this function's job; see TailWindow.
func Tail(ctx context.Context, db DB, after Cursor, limit int) ([]Event, error) {
	rows, err := db.Query(ctx,
		`SELECT `+eventColumns+`
		   FROM events
		  WHERE created_at >= $1
		    AND (created_at, id) > ($1, $2)
		  ORDER BY created_at, id
		  LIMIT $3`, after.At, after.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("tail events: %w", err)
	}
	return scanEvents(rows)
}

// TailWindow re-reads the recent past: everything from `from` up to and
// including the cursor.
//
// This is where the id-gap rule lives. bigserial ids are assigned BEFORE commit,
// so a transaction that takes its id early and commits late becomes visible
// after rows with higher positions have already been read ... and Tail, which
// only ever looks forward, will never return it. Sweeping the window behind the
// cursor and deduping by id is what catches it.
//
// A late commit is recent by construction, because created_at is clock_timestamp
// at insert and the row is visible within its own transaction's lifetime. So the
// window only has to be as wide as the longest transaction that writes events.
func TailWindow(ctx context.Context, db DB, from time.Time, to Cursor, limit int) ([]Event, error) {
	rows, err := db.Query(ctx,
		`SELECT `+eventColumns+`
		   FROM events
		  WHERE created_at >= $1
		    AND (created_at, id) <= ($2, $3)
		  ORDER BY created_at, id
		  LIMIT $4`, from, to.At, to.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("sweep events: %w", err)
	}
	return scanEvents(rows)
}

// Head returns the newest cursor in the log, or the zero cursor when it is
// empty. A subscriber with no Last-Event-ID starts here rather than replaying
// everything ever written.
func Head(ctx context.Context, db DB) (Cursor, error) {
	var c Cursor
	err := db.QueryRow(ctx,
		"SELECT created_at, id FROM events ORDER BY created_at DESC, id DESC LIMIT 1").Scan(&c.At, &c.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cursor{}, nil
	}
	if err != nil {
		return Cursor{}, fmt.Errorf("head: %w", err)
	}
	return c, nil
}

// Now returns the database clock. Every watermark in the bus is derived from
// it rather than from a host clock, so several hosts agree.
func Now(ctx context.Context, db DB) (time.Time, error) {
	var t time.Time
	if err := db.QueryRow(ctx, "SELECT now()").Scan(&t); err != nil {
		return time.Time{}, fmt.Errorf("db now: %w", err)
	}
	return t, nil
}

// Replay returns events strictly after `after` that the credential may see
// right now.
//
// "Right now" is the point: replay filters with CURRENT permissions, never
// permissions as of the event (D4.13). A revoked grant must not be replayed
// around, and because the filter runs inside visible_events() rather than in a
// WHERE clause assembled here, it cannot drift from the live path.
//
// Note what this does NOT pass: the owner. visible_events reads each event row
// itself, exactly as access_decision resolves a subject's owner, so there is no
// parameter a caller can get wrong. The `since` bound exists only to prune
// partitions and must be at or before after.At.
func (g *Guard) Replay(ctx context.Context, cred Credential, after Cursor, since time.Time, limit int) ([]Event, error) {
	rows, err := g.db.Query(ctx,
		`SELECT `+eventColumns+` FROM visible_events($1, $2, $3, $4, $5, $6, $7)`,
		since, after.At, after.ID,
		string(cred.PrincipalKind), cred.PrincipalID, cred.ActorID, limit)
	if err != nil {
		return nil, fmt.Errorf("replay events: %w", err)
	}
	return scanEvents(rows)
}

// Visible filters a batch of already-received events down to the ones this
// credential may see, in one round trip.
//
// This is the live path. One listening connection per host receives everything
// and the host filters per subscriber after receipt (D4.9), through the same
// rule the replay path uses.
//
// Only ids and timestamps go to the database. The rows are re-read there rather
// than trusting the copies held in memory, which costs one indexed lookup per
// event and removes any way for a stale or hand-built Event to talk its way
// past the filter.
func (g *Guard) Visible(ctx context.Context, cred Credential, events []Event) ([]Event, error) {
	if len(events) == 0 {
		return nil, nil
	}

	ids := make([]int64, len(events))
	createdAts := make([]time.Time, len(events))
	for i, e := range events {
		ids[i] = e.ID
		createdAts[i] = e.CreatedAt
	}

	rows, err := g.db.Query(ctx,
		`SELECT visible_event_ids($1, $2, $3, $4, $5)`,
		ids, createdAts, string(cred.PrincipalKind), cred.PrincipalID, cred.ActorID)
	if err != nil {
		return nil, fmt.Errorf("filter events: %w", err)
	}
	defer rows.Close()

	allowed := make(map[int64]struct{}, len(events))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan visible id: %w", err)
		}
		allowed[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("filter events: %w", err)
	}

	out := make([]Event, 0, len(allowed))
	for _, e := range events {
		if _, ok := allowed[e.ID]; ok {
			out = append(out, e)
		}
	}
	return out, nil
}
