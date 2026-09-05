//! Descriptors inside a document: the blobs a document names.

use std::collections::BTreeSet;

use hive_blob::Hash;
use hive_wasmhost::HostError;
use serde_json::Value;

/// Bounds on the walk. A document arrives from a guest, so both of these are
/// limits on what a guest can make the host do rather than tuning.
///
/// Neither truncates. Past a bound the write is refused, because a document
/// that silently kept only the first 256 of its descriptors would have
/// references for some of its blobs and not others, and the ones without would
/// be collected out from under a live document ... which is the exact failure
/// this whole file exists to prevent.
const MAX_DESCRIPTOR_DEPTH: usize = 64;
const MAX_DESCRIPTORS_PER_DOC: usize = 256;

/// The field a blob descriptor announces itself with, and it is RESERVED in a
/// document body. It is the wire name a `Descriptor` serialises with, so an app
/// that stores a descriptor stores this key whether or not it meant to.
const DESCRIPTOR_KEY: &str = "blob";

/// Every blob a document names, sorted and deduplicated.
///
/// # Why the match is loose
///
/// A descriptor on the wire is `{"blob": "<64 hex>", "size": N, "mime": "..."}`,
/// and the obvious tighter rule is to require all three. That rule was written
/// and rejected, because the two ways to be wrong here are not symmetric:
///
/// - Matching too little is silent and permanent. A blob a live document names
///   gets no reference, so it is unreferenced, so it is collected ... and the
///   document is corrupt at some later date with nothing connecting the two
///   events.
/// - Matching too much is loud and immediate. The write fails with a status the
///   caller sees, on the call that caused it.
///
/// So this matches on the presence of "blob" alone, and a value under it that is
/// not a 64-character hex digest is REFUSED rather than ignored. The refusal
/// names the JSON path, because "blob is reserved" is not actionable without
/// knowing which one. Sorted because serde_json's map is ordered, so the path
/// named first is the same on every retry.
pub fn descriptors_in(doc: &[u8]) -> Result<Vec<Hash>, HostError> {
    if doc.is_empty() {
        return Ok(Vec::new());
    }
    let root: Value = serde_json::from_slice(doc)
        .map_err(|e| HostError::invalid(format!("document is not json: {e}")))?;
    let mut out: BTreeSet<Hash> = BTreeSet::new();
    walk(&root, "doc", 0, &mut out)?;
    Ok(out.into_iter().collect())
}

fn walk(node: &Value, path: &str, depth: usize, out: &mut BTreeSet<Hash>) -> Result<(), HostError> {
    if depth > MAX_DESCRIPTOR_DEPTH {
        return Err(HostError::invalid(format!(
            "{path}: document nests deeper than {MAX_DESCRIPTOR_DEPTH} levels"
        )));
    }
    match node {
        Value::Object(map) => {
            if let Some(raw) = map.get(DESCRIPTOR_KEY) {
                let h = descriptor_hash(raw).map_err(|e| {
                    HostError::invalid(format!(
                        "{path}.{DESCRIPTOR_KEY}: {DESCRIPTOR_KEY:?} is reserved for blob descriptors and must be a 64-character hex digest ({e})"
                    ))
                })?;
                if !out.contains(&h) {
                    if out.len() >= MAX_DESCRIPTORS_PER_DOC {
                        return Err(HostError::invalid(format!(
                            "document names more than {MAX_DESCRIPTORS_PER_DOC} blobs"
                        )));
                    }
                    out.insert(h);
                }
            }
            // Keep descending even through an object that already matched. A
            // well-formed descriptor holds only scalars so this costs nothing,
            // and stopping would let a nested descriptor hide inside a
            // malformed one.
            for (key, child) in map {
                walk(child, &format!("{path}.{key}"), depth + 1, out)?;
            }
        }
        Value::Array(items) => {
            for (i, child) in items.iter().enumerate() {
                walk(child, &format!("{path}[{i}]"), depth + 1, out)?;
            }
        }
        _ => {}
    }
    Ok(())
}

/// Reads the hash out of the value under the reserved key. Every failure is an
/// error rather than a "not a descriptor", including a value of the wrong JSON
/// type: a number or an object under this key is not an innocent field the walk
/// should skip, it is the reserved word being used for something else.
fn descriptor_hash(raw: &Value) -> Result<Hash, String> {
    match raw {
        Value::String(s) => Hash::parse(s).map_err(|e| e.to_string()),
        other => Err(format!("value is {}, not a string", json_type(other))),
    }
}

fn json_type(v: &Value) -> &'static str {
    match v {
        Value::Null => "null",
        Value::Bool(_) => "bool",
        Value::Number(_) => "number",
        Value::String(_) => "string",
        Value::Array(_) => "array",
        Value::Object(_) => "object",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const GOOD: &str = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    /// The reserved key is a real constraint on app authors, so the refusal
    /// names the path, says the word is reserved, and says what a valid value
    /// looks like. An author hits this once.
    #[test]
    fn a_misused_reserved_key_is_refused_and_names_the_path() {
        let cases = [
            ("top level", r#"{"blob":"not a digest"}"#, "doc.blob"),
            ("nested", r#"{"cover":{"blob":"nope"}}"#, "doc.cover.blob"),
            (
                "inside an array",
                r#"{"files":[{"a":1},{"blob":"nope"}]}"#,
                "doc.files[1].blob",
            ),
            ("wrong type", r#"{"blob":123}"#, "doc.blob"),
            ("object", r#"{"blob":{"sha":"x"}}"#, "doc.blob"),
            ("null", r#"{"blob":null}"#, "doc.blob"),
            ("short hex", r#"{"blob":"abc123"}"#, "doc.blob"),
        ];
        for (name, doc, path) in cases {
            let err = descriptors_in(doc.as_bytes())
                .err()
                .unwrap_or_else(|| panic!("{name}: {doc} was accepted"));
            let msg = err.to_string();
            assert!(
                msg.contains(path),
                "{name}: the refusal does not name the path {path:?}: {msg}"
            );
            assert!(
                msg.contains("reserved"),
                "{name}: the refusal does not say the key is reserved: {msg}"
            );
        }
    }

    /// A digest that is one character wrong must fail, not become a string
    /// field, or the bytes it named would be collected under a live document.
    #[test]
    fn a_typo_in_a_digest_fails_rather_than_becoming_an_ordinary_field() {
        let hashes =
            descriptors_in(format!(r#"{{"file":{{"blob":"{GOOD}","size":3}}}}"#).as_bytes())
                .expect("valid");
        assert_eq!(hashes.len(), 1);
        let typo = format!("z{}", &GOOD[1..]);
        assert!(
            descriptors_in(format!(r#"{{"file":{{"blob":"{typo}","size":3}}}}"#).as_bytes())
                .is_err(),
            "a one-character typo in a digest was accepted as an ordinary field"
        );
    }

    /// A document with two bad paths must name the same one every time, and
    /// it is the FIRST path in sorted order.
    #[test]
    fn the_refusal_is_the_same_on_every_attempt() {
        let doc = br#"{"zzz":{"blob":"bad-z"},"aaa":{"blob":"bad-a"},"mmm":{"blob":"bad-m"}}"#;
        let first = descriptors_in(doc).err().expect("accepted").to_string();
        for i in 0..20 {
            let again = descriptors_in(doc)
                .err()
                .expect("accepted on a later attempt")
                .to_string();
            assert_eq!(again, first, "attempt {i} named a different path");
        }
        assert!(first.contains("doc.aaa.blob"), "{first}");
    }

    /// A 64-hex string under any other name is an ordinary field, or every
    /// app storing a checksum would be refused.
    #[test]
    fn only_the_reserved_key_is_reserved() {
        let doc = format!(r#"{{"checksum":"{GOOD}","sha256":"{GOOD}","note":"blob"}}"#);
        let hashes =
            descriptors_in(doc.as_bytes()).expect("a digest under an ordinary key was refused");
        assert!(hashes.is_empty());
    }

    /// Bounds refuse rather than truncate.
    #[test]
    fn bounds_refuse_rather_than_truncate() {
        let deep = format!(
            "{}1{}",
            r#"{"a":"#.repeat(MAX_DESCRIPTOR_DEPTH + 5),
            "}".repeat(MAX_DESCRIPTOR_DEPTH + 5)
        );
        let err = descriptors_in(deep.as_bytes())
            .err()
            .expect("a document past the depth bound was accepted");
        assert!(
            err.to_string().contains("nests deeper"),
            "refused for a different reason than depth: {err}"
        );

        // Distinct digests, because duplicates dedupe and would never reach
        // the bound.
        let mut distinct = std::collections::HashSet::new();
        let mut files = Vec::new();
        for i in 0..MAX_DESCRIPTORS_PER_DOC + 5 {
            let d = digest_for(i);
            distinct.insert(d.clone());
            files.push(format!(r#"{{"blob":"{d}"}}"#));
        }
        assert!(
            distinct.len() > MAX_DESCRIPTORS_PER_DOC,
            "fixture cannot exceed the bound"
        );
        let doc = format!(r#"{{"files":[{}]}}"#, files.join(","));
        let err = descriptors_in(doc.as_bytes())
            .err()
            .expect("a document naming too many blobs was accepted");
        assert!(
            err.to_string().contains("names more than"),
            "refused for a different reason than the count: {err}"
        );
    }

    fn digest_for(i: usize) -> String {
        const HEX: &[u8] = b"0123456789abcdef";
        let mut out: Vec<u8> = (0..64).map(|j| HEX[(i + j) % 16]).collect();
        out[0] = HEX[i % 16];
        out[1] = HEX[(i / 16) % 16];
        out[2] = HEX[(i / 256) % 16];
        String::from_utf8(out).unwrap()
    }
}
