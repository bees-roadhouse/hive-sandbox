//! The append-only log: the transport of record.

use std::fmt;
use std::sync::LazyLock;

use chrono::{DateTime, TimeZone, Utc};
use hive_identity::{Credential, Owner, PrincipalKind};
use regex::Regex;
use sqlx::{Executor, PgConnection, Postgres, Row};
use uuid::Uuid;

use crate::grants::{Guard, Subject, SubjectKind};
use crate::{Result, StoreError};

/// The format a kind must take, and it is the same expression the CHECK on
/// events.kind carries. Two copies is the deliberate trade named there: the
/// column is what holds for every writer including psql, and this is what gives
/// a caller an error naming the field rather than a constraint violation from
/// three layers down.
static EVENT_KIND: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[a-z0-9][a-z0-9._-]{0,127}$").expect("static regex"));

/// Rejects a kind before it can reach a subscriber's frame.
///
/// A kind is written into the `event:` field of an SSE frame, so a control
/// character in one splits a single event into two frames and lets the second
/// carry an `id:` the server had decided must not be written. Rejecting at the
/// writer is not a substitute for the column constraint and the column
/// constraint is not a substitute for this ... they fail for different callers.
pub fn valid_event_kind(kind: &str) -> Result<()> {
    if !EVENT_KIND.is_match(kind) {
        return Err(StoreError::BadEventKind(kind.to_string()));
    }
    Ok(())
}

/// A position in the events stream.
///
/// It is a PAIR, not an id, and that is forced by the schema: events is
/// partitioned by created_at, so there is no global index on id alone and an
/// id-only tail probes every partition on every poll. The timestamp is what
/// prunes partitions; the id is what breaks ties and makes the position exact.
///
/// See docs/events-tailing.md.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Cursor {
    /// `None` is the zero time: a cursor that names no timestamp.
    pub at: Option<DateTime<Utc>>,
    pub id: i64,
}

impl Cursor {
    pub fn new(at: DateTime<Utc>, id: i64) -> Cursor {
        Cursor { at: Some(at), id }
    }

    /// A cursor that names a time and no row: a watermark.
    pub fn at_time(at: DateTime<Utc>) -> Cursor {
        Cursor {
            at: Some(at),
            id: 0,
        }
    }

    /// Whether the cursor names no position.
    pub fn is_zero(&self) -> bool {
        self.id == 0 && self.at.is_none()
    }

    /// The timestamp, with the Unix epoch standing in for "no time". The Go
    /// tree's zero `time.Time` (year 1) played the same role; both sort below
    /// every row that will ever exist, and 1970 has the advantage of being a
    /// timestamptz Postgres can receive, where chrono's minimum is not.
    pub fn at_or_epoch(&self) -> DateTime<Utc> {
        self.at.unwrap_or(DateTime::UNIX_EPOCH)
    }

    /// Whether `self` sorts before `other` under (created_at, id).
    pub fn before(&self, other: &Cursor) -> bool {
        let (a, b) = (self.at_or_epoch(), other.at_or_epoch());
        if a != b {
            return a < b;
        }
        self.id < other.id
    }
}

impl fmt::Display for Cursor {
    /// Encodes the cursor for an SSE `id:` field: microseconds since epoch, a
    /// hyphen, then the row id. Clients treat it as opaque and hand it back.
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        if self.is_zero() {
            return Ok(());
        }
        let micros = self.at.map(|t| t.timestamp_micros()).unwrap_or(0);
        write!(f, "{micros}-{}", self.id)
    }
}

/// Decodes what a client sent back in Last-Event-ID.
///
/// A bare integer is accepted and yields an id with no timestamp, because an
/// older client (or a hand-written curl) may hold a pre-partitioning cursor.
/// The caller resolves the timestamp with one lookup; see [`resolve_cursor`].
pub fn parse_cursor(s: &str) -> Result<Cursor> {
    let s = s.trim();
    if s.is_empty() {
        return Ok(Cursor::default());
    }
    let bad = |what: &str| StoreError::InvalidInput(format!("cursor {s:?}: {what}"));
    match s.split_once('-') {
        None => {
            let n: i64 = s.parse().map_err(|_| bad("not an integer"))?;
            Ok(Cursor { at: None, id: n })
        }
        Some((micros, id)) => {
            let m: i64 = micros.parse().map_err(|_| bad("bad timestamp"))?;
            let n: i64 = id.parse().map_err(|_| bad("bad id"))?;
            let at = Utc
                .timestamp_micros(m)
                .single()
                .ok_or_else(|| bad("bad timestamp"))?;
            Ok(Cursor::new(at, n))
        }
    }
}

/// Fills in a missing timestamp for a bare-id cursor. Call it once per
/// connection, never per poll.
pub async fn resolve_cursor<'e, E>(db: E, c: Cursor) -> Result<Cursor>
where
    E: Executor<'e, Database = Postgres>,
{
    if c.id == 0 || c.at.is_some() {
        return Ok(c);
    }
    let at: Option<DateTime<Utc>> =
        sqlx::query_scalar("SELECT created_at FROM events WHERE id = $1")
            .bind(c.id)
            .fetch_optional(db)
            .await
            .map_err(|e| StoreError::db(format!("resolve cursor {}", c.id), e))?;
    match at {
        // The id is gone or was never ours. Treat it as "start from the
        // beginning of what we still keep" rather than guessing.
        None => Ok(Cursor::default()),
        Some(at) => Ok(Cursor::new(at, c.id)),
    }
}

/// One row of the append-only log. It is the transport of record: a NOTIFY
/// carrying its id is a wakeup bell, and every consumer stays correct if every
/// notification is dropped.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Event {
    pub id: i64,
    pub created_at: Option<DateTime<Utc>>,
    pub kind: String,
    /// What the event is about, in the shape `access_reason()` takes, so a
    /// replay filters through the same predicate as a live read. `None` for an
    /// event that names no grantable subject.
    pub subject: Option<Subject>,
    pub owner: Owner,
    pub author_actor: Uuid,
    pub principal_kind: PrincipalKind,
    pub principal_id: Uuid,
    /// Raw JSON.
    pub body: Vec<u8>,
    pub trust: String,
    pub cause_depth: i32,
    pub run_id: Option<Uuid>,
    pub origin: String,
    pub origin_id: Option<String>,
}

impl Event {
    /// A new event owned and authored by one credential's principal, with no
    /// subject. Everything else defaults the way `append_events` defaults it.
    pub fn new(kind: impl Into<String>, cred: &Credential, body: Vec<u8>) -> Event {
        Event {
            id: 0,
            created_at: None,
            kind: kind.into(),
            subject: None,
            owner: cred.owner_of(),
            author_actor: cred.actor_id,
            principal_kind: cred.principal_kind,
            principal_id: cred.principal_id,
            body,
            trust: String::new(),
            cause_depth: 0,
            run_id: None,
            origin: String::new(),
            origin_id: None,
        }
    }

    /// The event's own position.
    pub fn cursor(&self) -> Cursor {
        Cursor {
            at: self.created_at,
            id: self.id,
        }
    }
}

pub const EVENT_COLUMNS: &str = "id, created_at, kind, subject_kind, subject_id, subject_name,
    owner_kind, owner_id, author_actor, principal_kind, principal_id,
    body, trust, cause_depth, run_id, origin, origin_id";

pub(crate) fn scan_event(row: &sqlx::postgres::PgRow) -> Result<Event> {
    let subject_kind: Option<String> = row.get("subject_kind");
    let subject_id: Option<Uuid> = row.get("subject_id");
    let subject_name: Option<String> = row.get("subject_name");
    let owner_kind: String = row.get("owner_kind");
    let principal_kind: String = row.get("principal_kind");
    let body: serde_json::Value = row.get("body");
    let subject = match (
        subject_kind.as_deref().and_then(SubjectKind::parse),
        subject_id,
    ) {
        (Some(kind), Some(id)) => Some(Subject {
            kind,
            id,
            name: subject_name,
        }),
        _ => None,
    };
    Ok(Event {
        id: row.get("id"),
        created_at: Some(row.get("created_at")),
        kind: row.get("kind"),
        subject,
        owner: Owner::new(
            PrincipalKind::parse(&owner_kind)
                .ok_or_else(|| StoreError::Other(format!("owner kind {owner_kind:?}")))?,
            row.get("owner_id"),
        ),
        author_actor: row.get("author_actor"),
        principal_kind: PrincipalKind::parse(&principal_kind)
            .ok_or_else(|| StoreError::Other(format!("principal kind {principal_kind:?}")))?,
        principal_id: row.get("principal_id"),
        body: serde_json::to_vec(&body).unwrap_or_default(),
        trust: row.get("trust"),
        cause_depth: row.get("cause_depth"),
        run_id: row.get("run_id"),
        origin: row.get("origin"),
        origin_id: row.get("origin_id"),
    })
}

fn scan_events(rows: Vec<sqlx::postgres::PgRow>) -> Result<Vec<Event>> {
    rows.iter().map(scan_event).collect()
}

/// The one coarse channel (D4.9). Visibility is filtered in the host after
/// receipt, not by splitting channels: authorization is domain logic and belongs
/// with domain logic.
pub const NOTIFY_CHANNEL: &str = "hive_events";

/// Inserts events and issues exactly ONE notification for the whole call,
/// carrying the highest cursor written. Each event's `id` and `created_at` are
/// filled in.
///
/// One notify per unit of work is not a micro-optimisation: NOTIFY takes a
/// heavy lock at commit and serialises commits, so a per-row notify would cost
/// the whole write path, not just the notification path (D4.11, D4.17).
///
/// Pass a transaction when the events have to land with other writes. A
/// journal entry, its mention row and its read grant are one transaction by
/// rule (D13.2), and the event belongs in it.
pub async fn append_events(conn: &mut PgConnection, events: &mut [Event]) -> Result<()> {
    if events.is_empty() {
        return Ok(());
    }
    let mut highest = Cursor::default();
    for e in events.iter_mut() {
        valid_event_kind(&e.kind)?;
        if e.origin.is_empty() {
            e.origin = "local".into();
        }
        if e.trust.is_empty() {
            e.trust = "trusted".into();
        }
        if e.body.is_empty() {
            e.body = b"{}".to_vec();
        }
        let body: serde_json::Value = serde_json::from_slice(&e.body)
            .map_err(|err| StoreError::InvalidInput(format!("event body is not json: {err}")))?;
        let (subject_kind, subject_id, subject_name) = match &e.subject {
            Some(s) => (Some(s.kind.as_str()), Some(s.id), s.name.as_deref()),
            None => (None, None, None),
        };
        let row = sqlx::query(
            "INSERT INTO events (kind, subject_kind, subject_id, subject_name,
                                 owner_kind, owner_id, author_actor,
                                 principal_kind, principal_id,
                                 body, trust, cause_depth, run_id, origin, origin_id)
             VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
             RETURNING id, created_at",
        )
        .bind(&e.kind)
        .bind(subject_kind)
        .bind(subject_id)
        .bind(subject_name)
        .bind(e.owner.kind.as_str())
        .bind(e.owner.id)
        .bind(e.author_actor)
        .bind(e.principal_kind.as_str())
        .bind(e.principal_id)
        .bind(&body)
        .bind(&e.trust)
        .bind(e.cause_depth)
        .bind(e.run_id)
        .bind(&e.origin)
        .bind(&e.origin_id)
        .fetch_one(&mut *conn)
        .await
        .map_err(|err| StoreError::db(format!("append event {:?}", e.kind), err))?;
        e.id = row.get("id");
        e.created_at = Some(row.get("created_at"));
        if highest.before(&e.cursor()) {
            highest = e.cursor();
        }
    }
    // The payload is a hint and nothing more. A consumer that never sees it
    // still catches up on its next poll.
    sqlx::query("SELECT pg_notify($1, $2)")
        .bind(NOTIFY_CHANNEL)
        .bind(highest.to_string())
        .execute(&mut *conn)
        .await
        .map_err(|e| StoreError::db("notify", e))?;
    Ok(())
}

/// Reads forward from a cursor: the host's view of the log, unfiltered.
///
/// Strictly after the cursor, so a full batch always makes progress. An earlier
/// version took a bare timestamp derived from the newest row DELIVERED, which
/// meant a burst larger than one batch produced a query that returned the same
/// oldest rows forever. The cursor's timestamp is also the lower bound, which is
/// what prunes partitions. Rows that commit late with a position BELOW the
/// cursor are not this function's job; see [`tail_window`].
pub async fn tail<'e, E>(db: E, after: Cursor, limit: i64) -> Result<Vec<Event>>
where
    E: Executor<'e, Database = Postgres>,
{
    let rows = sqlx::query(&format!(
        "SELECT {EVENT_COLUMNS}
           FROM events
          WHERE created_at >= $1
            AND (created_at, id) > ($1, $2)
          ORDER BY created_at, id
          LIMIT $3"
    ))
    .bind(after.at_or_epoch())
    .bind(after.id)
    .bind(limit)
    .fetch_all(db)
    .await
    .map_err(|e| StoreError::db("tail events", e))?;
    scan_events(rows)
}

/// Re-reads the recent past: everything from `from` up to and including the
/// cursor.
///
/// This is where the id-gap rule lives. bigserial ids are assigned BEFORE
/// commit, so a transaction that takes its id early and commits late becomes
/// visible after rows with higher positions have already been read ... and
/// `tail`, which only ever looks forward, will never return it. Sweeping the
/// window behind the cursor and deduping by id is what catches it.
pub async fn tail_window<'e, E>(
    db: E,
    from: DateTime<Utc>,
    to: Cursor,
    limit: i64,
) -> Result<Vec<Event>>
where
    E: Executor<'e, Database = Postgres>,
{
    let rows = sqlx::query(&format!(
        "SELECT {EVENT_COLUMNS}
           FROM events
          WHERE created_at >= $1
            AND (created_at, id) <= ($2, $3)
          ORDER BY created_at, id
          LIMIT $4"
    ))
    .bind(from)
    .bind(to.at_or_epoch())
    .bind(to.id)
    .bind(limit)
    .fetch_all(db)
    .await
    .map_err(|e| StoreError::db("sweep events", e))?;
    scan_events(rows)
}

/// The newest cursor in the log, or the zero cursor when it is empty. A
/// subscriber with no Last-Event-ID starts here rather than replaying everything
/// ever written.
pub async fn head<'e, E>(db: E) -> Result<Cursor>
where
    E: Executor<'e, Database = Postgres>,
{
    let row =
        sqlx::query("SELECT created_at, id FROM events ORDER BY created_at DESC, id DESC LIMIT 1")
            .fetch_optional(db)
            .await
            .map_err(|e| StoreError::db("head", e))?;
    Ok(match row {
        None => Cursor::default(),
        Some(r) => Cursor::new(r.get("created_at"), r.get("id")),
    })
}

/// The database clock. Every watermark in the bus is derived from it rather
/// than from a host clock, so several hosts agree.
pub async fn now<'e, E>(db: E) -> Result<DateTime<Utc>>
where
    E: Executor<'e, Database = Postgres>,
{
    sqlx::query_scalar("SELECT now()")
        .fetch_one(db)
        .await
        .map_err(|e| StoreError::db("db now", e))
}

impl Guard {
    /// Events strictly after `after` that the credential may see right now.
    ///
    /// "Right now" is the point: replay filters with CURRENT permissions, never
    /// permissions as of the event (D4.13). A revoked grant must not be replayed
    /// around, and because the filter runs inside `visible_events()` rather than
    /// in a WHERE clause assembled here, it cannot drift from the live path.
    ///
    /// Note what this does NOT pass: the owner. `visible_events` reads each
    /// event row itself, exactly as `access_decision` resolves a subject's
    /// owner, so there is no parameter a caller can get wrong. The `since` bound
    /// exists only to prune partitions and must be at or before `after.at`.
    pub async fn replay(
        &self,
        db: &mut PgConnection,
        cred: &Credential,
        after: Cursor,
        since: DateTime<Utc>,
        limit: i64,
    ) -> Result<Vec<Event>> {
        let rows = sqlx::query(&format!(
            "SELECT {EVENT_COLUMNS} FROM visible_events($1, $2, $3, $4, $5, $6, $7)"
        ))
        .bind(since)
        .bind(after.at_or_epoch())
        .bind(after.id)
        .bind(cred.principal_kind.as_str())
        .bind(cred.principal_id)
        .bind(cred.actor_id)
        .bind(limit as i32)
        .fetch_all(&mut *db)
        .await
        .map_err(|e| StoreError::db("replay events", e))?;
        scan_events(rows)
    }

    /// Filters a batch of already-received events down to the ones this
    /// credential may see, in one round trip.
    ///
    /// This is the live path. One listening connection per host receives
    /// everything and the host filters per subscriber after receipt (D4.9),
    /// through the same rule the replay path uses.
    ///
    /// Only ids and timestamps go to the database. The rows are re-read there
    /// rather than trusting the copies held in memory, which removes any way
    /// for a stale or hand-built event to talk its way past the filter.
    pub async fn visible(
        &self,
        db: &mut PgConnection,
        cred: &Credential,
        events: &[Event],
    ) -> Result<Vec<Event>> {
        if events.is_empty() {
            return Ok(Vec::new());
        }
        let ids: Vec<i64> = events.iter().map(|e| e.id).collect();
        let created: Vec<DateTime<Utc>> = events.iter().map(|e| e.at_or_epoch()).collect();
        let allowed: Vec<i64> = sqlx::query_scalar("SELECT visible_event_ids($1, $2, $3, $4, $5)")
            .bind(&ids)
            .bind(&created)
            .bind(cred.principal_kind.as_str())
            .bind(cred.principal_id)
            .bind(cred.actor_id)
            .fetch_all(&mut *db)
            .await
            .map_err(|e| StoreError::db("filter events", e))?;
        Ok(events
            .iter()
            .filter(|e| allowed.contains(&e.id))
            .cloned()
            .collect())
    }
}

impl Event {
    fn at_or_epoch(&self) -> DateTime<Utc> {
        self.created_at.unwrap_or(DateTime::UNIX_EPOCH)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cursor_string_round_trips() {
        let c = Cursor::new(Utc.timestamp_micros(1_700_000_000_123_456).unwrap(), 42);
        let s = c.to_string();
        assert_eq!(s, "1700000000123456-42");
        assert_eq!(parse_cursor(&s).unwrap(), c);
        assert_eq!(Cursor::default().to_string(), "");
        assert_eq!(parse_cursor("").unwrap(), Cursor::default());
        // A bare id is an accepted, unresolved cursor.
        assert_eq!(parse_cursor("17").unwrap(), Cursor { at: None, id: 17 });
        assert!(parse_cursor("x-1").is_err());
        assert!(parse_cursor("1-x").is_err());
    }

    #[test]
    fn kinds_are_dotted_identifiers() {
        assert!(valid_event_kind("journal.entry.created").is_ok());
        assert!(valid_event_kind("evil\nid: 9").is_err());
        assert!(valid_event_kind("").is_err());
        assert!(valid_event_kind(".leading").is_err());
    }
}
