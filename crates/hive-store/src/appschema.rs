//! Provisioning an app's storage from a manifest.
//!
//! The split is deliberate. The manifest crate derives a `SchemaPlan`, which is
//! data: printable, diffable, and testable without a database. This module is
//! the only thing that turns that data into statements, because the store is the
//! one crate in the platform that talks to Postgres.
//!
//! That is not tidiness. The grant predicate lives here, and a second crate
//! holding a pool would be a second crate reaching the database without any
//! reason to know about grants ... which is precisely how the first hole gets
//! made (invariant 1, and D21's shape).
//!
//! Nothing in here interpolates a string that came from a manifest without
//! having been through `parse_index` or the identifier check below. A manifest
//! is a file an AI writes; DDL built from one is the obvious injection surface,
//! and quoting at this end is the last line rather than the only one.

use hive_manifest::{CollectionPlan, Index, IndexMethod, SchemaPlan};
use sqlx::PgConnection;

use crate::{Result, StoreError};

/// Postgres's limit, and the reason `derived_ident` exists.
const MAX_IDENTIFIER: usize = 63;

/// Provisions an app's schema, its collection tables and its indexes. It is
/// idempotent: re-applying the same plan is how a manifest diff becomes a
/// migration (D3.3), so every statement is IF NOT EXISTS.
///
/// It runs inside the caller's transaction, so a failed install leaves nothing
/// behind, and it never commits ... registering an install and provisioning its
/// storage are one unit of work or they are a schema nobody owns.
pub async fn apply_schema_plan(tx: &mut PgConnection, plan: &SchemaPlan) -> Result<()> {
    check_ident(&plan.schema)?;
    let schema = quote_ident(&plan.schema);
    sqlx::query(&format!("CREATE SCHEMA IF NOT EXISTS {schema}"))
        .execute(&mut *tx)
        .await
        .map_err(|e| StoreError::db(format!("create schema {}", plan.schema), e))?;
    if !plan.collections.is_empty() {
        apply_touch_function(tx, &schema).await?;
    }
    for c in &plan.collections {
        apply_collection(tx, &plan.schema, c).await?;
    }
    Ok(())
}

/// Uninstall. One statement, because per-app schemas exist so that the blast
/// radius of a bad app is exactly this (D3.2).
pub async fn drop_schema_plan(tx: &mut PgConnection, plan: &SchemaPlan) -> Result<()> {
    check_ident(&plan.schema)?;
    sqlx::query(&format!(
        "DROP SCHEMA IF EXISTS {} CASCADE",
        quote_ident(&plan.schema)
    ))
    .execute(&mut *tx)
    .await
    .map_err(|e| StoreError::db(format!("drop schema {}", plan.schema), e))?;
    Ok(())
}

/// Installs the updated_at trigger function into the app's own schema.
///
/// Per-app rather than platform-wide so that DROP SCHEMA CASCADE remains the
/// whole of uninstall (D3.2). A shared function would make every app's triggers
/// depend on an object outside their blast radius.
async fn apply_touch_function(tx: &mut PgConnection, quoted_schema: &str) -> Result<()> {
    sqlx::query(&format!(
        "CREATE OR REPLACE FUNCTION {quoted_schema}.set_updated_at()
         RETURNS trigger LANGUAGE plpgsql AS $$
         BEGIN
             NEW.updated_at = now();
             RETURN NEW;
         END;
         $$"
    ))
    .execute(&mut *tx)
    .await
    .map_err(|e| StoreError::db(format!("create set_updated_at in {quoted_schema}"), e))?;
    Ok(())
}

/// Creates one collection's table, its updated_at trigger and its indexes. The
/// table shape is the same for every collection and is not the app's to choose.
async fn apply_collection(tx: &mut PgConnection, schema: &str, c: &CollectionPlan) -> Result<()> {
    check_ident(&c.name)?;
    let table = format!("{}.{}", quote_ident(schema), quote_ident(&c.name));

    // There is deliberately no owner pair and no author here.
    //
    // A document's ownership lives on its `entities` row, which is also what a
    // grant is written against and what the predicate resolves through. A copy
    // on this table would be a second place to read ownership from, and the
    // only thing stopping a query filtering on the cheaper copy would be a
    // comment ... which is attention rather than intent.
    //
    // The id IS the entity's id, and there is deliberately no foreign key
    // saying so either: an app schema has to be provisionable without the
    // platform's tables in the same search path. What keeps the two rows
    // together is that one transaction writes both and one removes both.
    //
    // trust IS duplicated from `entities`, and that is not the same case: it
    // travels with the row (invariant 3), it is read by the layer serving the
    // document, and nothing authorizes on it.
    sqlx::query(&format!(
        "CREATE TABLE IF NOT EXISTS {table} (
            id          uuid PRIMARY KEY,
            doc         jsonb NOT NULL DEFAULT '{{}}',
            trust       text NOT NULL DEFAULT 'trusted' CHECK (trust IN ('trusted', 'untrusted')),
            tainted_by  text,
            created_at  timestamptz NOT NULL DEFAULT now(),
            updated_at  timestamptz NOT NULL DEFAULT now()
        )"
    ))
    .execute(&mut *tx)
    .await
    .map_err(|e| StoreError::db(format!("create table {schema}.{}", c.name), e))?;

    // updated_at IS maintained by a trigger, and this is the one place the
    // project's usual "no triggers" instinct does not apply. That instinct
    // comes from D21: a trigger cannot enforce what the writer supplies, because
    // a trigger has no credential in scope. Entirely correct, and it says
    // nothing about this column, because `now()` is not a fact the writer
    // supplies ... it is a clock read, identical whoever is asking.
    let trigger = derived_ident(&c.name, "_touch")?;
    sqlx::query(&format!("DROP TRIGGER IF EXISTS {trigger} ON {table}"))
        .execute(&mut *tx)
        .await
        .map_err(|e| StoreError::db(format!("drop touch trigger on {schema}.{}", c.name), e))?;
    sqlx::query(&format!(
        "CREATE TRIGGER {trigger} BEFORE UPDATE ON {table} FOR EACH ROW EXECUTE FUNCTION {}.set_updated_at()",
        quote_ident(schema)
    ))
    .execute(&mut *tx)
    .await
    .map_err(|e| StoreError::db(format!("create touch trigger on {schema}.{}", c.name), e))?;

    for (i, idx) in c.indexes.iter().enumerate() {
        apply_index(tx, schema, &c.name, &table, i, idx).await?;
    }
    Ok(())
}

async fn apply_index(
    tx: &mut PgConnection,
    schema: &str,
    collection: &str,
    table: &str,
    ordinal: usize,
    idx: &Index,
) -> Result<()> {
    let expr = doc_path(idx)?;
    // The index name is derived rather than taken from the manifest, so two
    // apps cannot argue about it and an app cannot name one after something
    // that already exists.
    let name = derived_ident(collection, &format!("_{}_{ordinal}_idx", idx.method))?;
    let stmt = match idx.method {
        IndexMethod::BTree => format!("CREATE INDEX IF NOT EXISTS {name} ON {table} (({expr}))"),
        IndexMethod::Gin => {
            format!("CREATE INDEX IF NOT EXISTS {name} ON {table} USING gin (({expr}))")
        }
        // to_tsvector needs a regconfig and a text argument. The config is a
        // constant here rather than an app's choice: a manifest that could pick
        // one could pick anything, and per-language configuration is a real
        // decision that has not been made yet.
        IndexMethod::Fts => format!(
            "CREATE INDEX IF NOT EXISTS {name} ON {table} USING gin (to_tsvector('english', {expr}))"
        ),
        // Vector wants a typed column rather than a jsonb expression. Refused
        // loudly rather than half-built: a silently skipped index is a query
        // plan that quietly falls back to a sequential scan over someone's
        // whole memory, discovered months later as "search got slow". The
        // provisional pick is hnsw, on the access pattern: ivfflat needs
        // training data and degrades as the corpus outgrows it, which is
        // exactly the shape of a journal that starts empty and grows forever.
        IndexMethod::Vector => {
            return Err(StoreError::NotImplemented(format!(
                "vector indexes need a typed column and an index method nobody has chosen yet ({schema}.{collection}: {idx})"
            )));
        }
    };
    sqlx::query(&stmt).execute(&mut *tx).await.map_err(|e| {
        StoreError::db(
            format!("create {} index on {schema}.{collection}", idx.method),
            e,
        )
    })?;
    Ok(())
}

/// Builds the jsonb accessor for an index path.
///
/// Each segment is a literal inside the expression, so each one is quoted as a
/// string literal rather than concatenated raw. `parse_index` has already
/// restricted segments to `[a-z][a-z0-9_]*`, so there is nothing to escape ...
/// which is exactly why the check below is cheap enough to keep.
fn doc_path(idx: &Index) -> Result<String> {
    if idx.path.is_empty() {
        return Err(StoreError::UnsafeIdentifier("index with no path".into()));
    }
    for seg in &idx.path {
        check_ident(seg)?;
    }
    // ->> yields text at the last hop, -> yields jsonb along the way. btree and
    // fts want text; gin over a tag array wants the jsonb.
    let mut expr = String::from("doc");
    let last = idx.path.len() - 1;
    for (i, seg) in idx.path.iter().enumerate() {
        let op = if idx.method != IndexMethod::Gin && i == last {
            " ->> "
        } else {
            " -> "
        };
        expr.push_str(op);
        expr.push_str(&quote_literal(seg));
    }
    Ok(expr)
}

/// The same shape the manifest's validation enforces, duplicated on purpose:
/// this is the check at the POINT OF USE, and a check that trusts an earlier one
/// is a check that stops running the day somebody adds a second caller that
/// skipped it.
pub(crate) fn check_ident(s: &str) -> Result<()> {
    let ok = !s.is_empty()
        && s.len() <= MAX_IDENTIFIER
        && s.chars()
            .enumerate()
            .all(|(i, c)| c.is_ascii_lowercase() || (i > 0 && (c.is_ascii_digit() || c == '_')));
    if !ok {
        return Err(StoreError::UnsafeIdentifier(format!("{s:?}")));
    }
    Ok(())
}

/// Builds an identifier the platform derives from a manifest name, and REFUSES
/// one that would not fit rather than letting Postgres truncate it.
///
/// Truncation is the dangerous half. An over-long index name collapses onto the
/// collection name, which the table already occupies in pg_class, and the IF
/// NOT EXISTS that makes re-apply idempotent turns that collision into a
/// NOTICE. The driver does not surface NOTICEs, so the statement reports success
/// and the index does not exist.
fn derived_ident(base: &str, suffix: &str) -> Result<String> {
    check_ident(base)?;
    let name = format!("{base}{suffix}");
    if name.len() > MAX_IDENTIFIER {
        return Err(StoreError::UnsafeIdentifier(format!(
            "{name:?} is {} characters and Postgres truncates at {MAX_IDENTIFIER}, which would silently collide with an existing object",
            name.len()
        )));
    }
    Ok(quote_ident(&name))
}

/// Double-quotes an identifier. Everything reaching it has already passed
/// `check_ident`, so the doubling is belt on brace.
pub(crate) fn quote_ident(s: &str) -> String {
    format!("\"{}\"", s.replace('"', "\"\""))
}

/// Single-quotes a string literal for embedding in an expression.
fn quote_literal(s: &str) -> String {
    format!("'{}'", s.replace('\'', "''"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn idents_are_bounded_and_lowercase() {
        assert!(check_ident("entries").is_ok());
        assert!(check_ident("e_2").is_ok());
        assert!(check_ident("").is_err());
        assert!(check_ident("Entries").is_err());
        assert!(check_ident("_x").is_err());
        assert!(check_ident("x\"; drop").is_err());
        assert!(check_ident(&"a".repeat(64)).is_err());
    }

    #[test]
    fn derived_names_refuse_to_truncate() {
        assert!(derived_ident(&"c".repeat(63), "_touch").is_err());
        assert_eq!(derived_ident("c", "_touch").unwrap(), "\"c_touch\"");
    }

    #[test]
    fn doc_paths_quote_every_segment() {
        let idx = hive_manifest::parse_index("btree(author.name)").unwrap();
        assert_eq!(doc_path(&idx).unwrap(), "doc -> 'author' ->> 'name'");
        let gin = hive_manifest::parse_index("gin(tags)").unwrap();
        assert_eq!(doc_path(&gin).unwrap(), "doc -> 'tags'");
    }
}
