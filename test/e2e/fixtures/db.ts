import { randomBytes, randomUUID } from 'node:crypto';

import { Client } from 'pg';

/**
 * Postgres for the e2e suite.
 *
 * Every worker gets its own schema on the shared test database and the daemon
 * migrates into it, so workers cannot see each other's events. That matters
 * more here than in the Rust tests: an SSE spec asserts on what a stream did NOT
 * deliver, and a stray event from another worker would look like a filtering
 * bug.
 */
export const DATABASE_URL_ENV = 'HIVE_SANDBOX_TEST_DATABASE_URL';

export function databaseURL(): string {
  const url = (process.env[DATABASE_URL_ENV] ?? '').trim();
  if (url === '') {
    throw new Error(
      `${DATABASE_URL_ENV} is not set. Run scripts/db-up.ps1 (or .sh) and export what it prints.`,
    );
  }
  return url;
}

/** A connection string pinned to one schema, for the daemon to run against. */
export function scopedURL(url: string, schema: string): string {
  const u = new URL(url);
  // libpq's `options` is how a search_path travels in a connection string;
  // pgx passes it through to the server as a startup parameter.
  u.searchParams.set('options', `-c search_path=${schema}`);
  return u.toString();
}

export function newSchemaName(): string {
  return `e2e_${randomBytes(6).toString('hex')}`;
}

export interface Schema {
  name: string;
  /** Connection string the daemon should use. */
  url: string;
  drop(): Promise<void>;
}

export async function createSchema(): Promise<Schema> {
  const base = databaseURL();
  const name = newSchemaName();

  const admin = new Client({ connectionString: base });
  await admin.connect();
  try {
    await admin.query(`create schema "${name}"`);
  } finally {
    await admin.end();
  }

  return {
    name,
    url: scopedURL(base, name),
    drop: async () => {
      const client = new Client({ connectionString: base });
      await client.connect();
      try {
        await client.query(`drop schema if exists "${name}" cascade`);
      } finally {
        await client.end();
      }
    },
  };
}

/**
 * A writer that appends events exactly the way any other writer would.
 *
 * Deliberately NOT going through the daemon: the design claim is that the
 * events table is the transport and anything that writes a row and notifies is
 * a publisher. Driving the stream from outside the process under test is what
 * makes that claim testable rather than assumed.
 */
export class EventWriter {
  private constructor(
    private readonly client: Client,
    private readonly actorID: string,
  ) {}

  static async connect(schema: Schema): Promise<EventWriter> {
    const client = new Client({ connectionString: databaseURL() });
    await client.connect();
    await client.query(`set search_path to "${schema.name}"`);

    // The daemon bootstrapped the root actor; events need an owner and an
    // author, and this is the one the e2e token authenticates as.
    const res = await client.query<{ id: string }>(
      'select id from actors where created_by_actor is null',
    );
    const root = res.rows[0];
    if (res.rowCount !== 1 || root === undefined) {
      throw new Error(`expected exactly one root actor, found ${String(res.rowCount)}`);
    }
    return new EventWriter(client, root.id);
  }

  /** The root actor, which owns everything this writer appends. */
  get owner(): string {
    return this.actorID;
  }

  /** Appends one event and notifies once, the same as store.AppendEvents. */
  async append(kind: string, body: Record<string, unknown> = {}, owner = this.actorID): Promise<string> {
    const res = await this.client.query<{ id: string; created_at: Date }>(
      `insert into events (kind, owner_kind, owner_id, author_actor, principal_kind, principal_id, body)
       values ($1, 'user', $2, $3, 'user', $3, $4)
       returning id, created_at`,
      [kind, owner, this.actorID, JSON.stringify(body)],
    );
    const row = res.rows[0];
    if (row === undefined) {
      throw new Error(`insert of ${kind} returned no row`);
    }
    const micros = row.created_at.getTime() * 1000;
    await this.client.query('select pg_notify($1, $2)', ['hive_events', `${micros}-${row.id}`]);
    return row.id;
  }

  /**
   * Appends an event and does NOT notify.
   *
   * Invariant 4 says the events table is the transport and NOTIFY is only a
   * wakeup bell, so a consumer has to stay correct when every notification is
   * dropped. This is the half of that claim which is easy to write down and
   * easy to never test: the only thing that can deliver this row is the
   * backstop poll.
   */
  async appendWithoutNotify(kind: string, body: Record<string, unknown> = {}): Promise<string> {
    const res = await this.client.query<{ id: string }>(
      `insert into events (kind, owner_kind, owner_id, author_actor, principal_kind, principal_id, body)
       values ($1, 'user', $2, $2, 'user', $2, $3)
       returning id`,
      [kind, this.actorID, JSON.stringify(body)],
    );
    const row = res.rows[0];
    if (row === undefined) {
      throw new Error(`insert of ${kind} returned no row`);
    }
    return row.id;
  }

  /**
   * Appends an event owned by somebody else, so a spec can assert on what a
   * stream EXCLUDES. events.owner_id carries no foreign key (the table is
   * partitioned and append-only, and a detachable partition should not be
   * pinned by references), so a bare uuid is enough to stand in for another
   * principal here.
   */
  async appendForeign(kind: string): Promise<string> {
    return this.append(kind, {}, randomUUID());
  }

  async close(): Promise<void> {
    await this.client.end();
  }
}
