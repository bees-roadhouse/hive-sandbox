//! The events capability for guest apps: `hive_events.emit`.
//!
//! It lives here rather than in a crate of its own because appending an event
//! is a write, and invariant 1 puts every write behind ONE enforcement point in
//! this layer. An adapter elsewhere would be a second place that decides who may
//! write, which is the thing that invariant exists to prevent.

use async_trait::async_trait;
use hive_wasmhost::{Events, HostError, Request, Response};
use serde::Deserialize;

use crate::appdata::resolve_active_install;
use crate::events::{Event, append_events, valid_event_kind};
use crate::grants::{Access, Subject};
use crate::{Store, StoreError};

/// What every guest-emitted kind is filed under.
///
/// The namespace is NOT decoration. `valid_event_kind` checks only the SHAPE of
/// a kind ... its job is stopping a control character from splitting an SSE
/// frame ... so without a prefix any guest could emit `journal.entry.created`
/// or `storage.insert` and every subscriber would act on a fabricated event.
///
/// It is invariant 14 in the small: a bare kind omits the dimension its
/// correctness depends on, which is WHO emitted it. So the host supplies that
/// dimension and the guest cannot: the prefix is derived from the install row
/// resolved out of the caller's credential, never from the request body.
const KIND_PREFIX: &str = "app.";

pub struct GuestEvents {
    store: Store,
}

/// What a guest sends. Note what is ABSENT: no actor, no principal, no owner,
/// no trust, no origin. Every one of those is resolved host-side from the
/// credential, because a body is data and invariant 2 says the authority pair
/// comes from the credential.
#[derive(Deserialize, Default)]
#[serde(default)]
struct EmitBody {
    kind: String,
    body: Option<serde_json::Value>,
}

/// The shape a guest may ask for: the part AFTER the namespace. Deliberately
/// narrower than `valid_event_kind`'s own pattern ... no leading dot, no way to
/// climb out of the prefix.
fn guest_kind(k: &str) -> Result<(), HostError> {
    if k.is_empty() {
        return Err(HostError::invalid("events.emit: kind is required"));
    }
    if k.len() > 96 {
        return Err(HostError::invalid("events.emit: kind is too long"));
    }
    let ok = k.chars().enumerate().all(|(i, c)| {
        c.is_ascii_lowercase() || c.is_ascii_digit() || (i > 0 && matches!(c, '.' | '_' | '-'))
    });
    if !ok {
        return Err(HostError::invalid(format!(
            "events.emit: kind {k:?} must be lower-case alphanumeric with . _ -"
        )));
    }
    Ok(())
}

impl GuestEvents {
    pub fn new(store: Store) -> GuestEvents {
        GuestEvents { store }
    }

    /// Appends one event on behalf of a guest app.
    ///
    /// Everything that says WHO is host-derived; everything the guest supplies
    /// is treated as data. The kind it asks for is filed under its own
    /// namespace, so a guest can raise events about itself and cannot raise one
    /// that another app or the platform would be believed to have raised.
    async fn emit_inner(&self, req: &Request) -> Result<Response, HostError> {
        req.caller
            .validate()
            .map_err(|e| HostError::denied(format!("events.emit: {e}")))?;
        // Deliberately does NOT echo the decoder's message. Every error string
        // is copied into the guest's result slot, so echoing one hands back a
        // fragment of whatever was being decoded ... and the guest is not
        // always the party that supplied it.
        let input: EmitBody = serde_json::from_slice(&req.body)
            .map_err(|_| HostError::invalid("events.emit: body is not an object"))?;
        guest_kind(&input.kind)?;
        let body = input.body.unwrap_or_else(|| serde_json::json!({}));

        let mut tx = self.store.begin().await.map_err(host)?;
        let info = resolve_active_install(&mut *tx, req.caller.install_id)
            .await
            .map_err(host)?;
        // The namespace comes from the install row, not from req.app. Both are
        // host-filled, but the row is the one that cannot drift from what is
        // actually installed.
        let kind = format!("{KIND_PREFIX}{}.{}", info.slug, input.kind);
        valid_event_kind(&kind).map_err(|e| HostError::invalid(format!("events.emit: {e}")))?;

        // Emitting is a write against the install, and the predicate decides
        // it. An app writing its own events reads 'owner' here; anything else
        // needs a grant. Absence of scope is deny (invariant 1).
        let subject = Subject::install(info.id);
        self.store
            .guard()
            .authorize(
                &mut tx,
                &req.caller.cred,
                &subject,
                Access::Write,
                "events.emit",
            )
            .await
            .map_err(host)?;

        let mut ev = Event::new(kind.clone(), &req.caller.cred, serde_json::to_vec(&body)?);
        ev.subject = Some(subject);
        ev.owner = info.owner;
        // Verbatim from the invocation. append_events defaults an empty trust
        // to "trusted", so a forgotten assignment here would launder taint at
        // the last possible moment ... invariant 9 breaking after every other
        // layer got it right.
        ev.trust = req.trust.as_str().to_string();
        ev.origin = "guest".into();
        append_events(&mut tx, std::slice::from_mut(&mut ev))
            .await
            .map_err(host)?;
        tx.commit()
            .await
            .map_err(|e| HostError::error(e.to_string()))?;

        // The response can never be more trusted than the invocation that
        // produced it. This is a write: it reports what was recorded.
        Ok(Response::with_trust(
            req.trust,
            serde_json::to_vec(&serde_json::json!({"kind": kind}))?,
        ))
    }
}

fn host(e: StoreError) -> HostError {
    match e {
        StoreError::Host(h) => h,
        StoreError::Denied => HostError::denied("denied"),
        other => HostError::error(other.to_string()),
    }
}

#[async_trait]
impl Events for GuestEvents {
    async fn emit(&self, req: Request) -> Result<Response, HostError> {
        self.emit_inner(&req).await
    }
}

/// The kind prefix an install's events are filed under.
fn namespace_of(slug: &str) -> String {
    format!("{KIND_PREFIX}{slug}.")
}

/// Whether a kind is one the platform itself raises, as opposed to a guest
/// app's. Guests may subscribe to their own namespace and to platform kinds, and
/// to nothing else.
pub fn platform_kind(kind: &str) -> bool {
    !kind.starts_with(KIND_PREFIX)
}

/// Whether an install may see an event of this kind: its own namespace, or the
/// platform's. Another app's events are not visible without a grant, and there
/// is no way to ask for them here.
pub fn visible_to(kind: &str, slug: &str) -> bool {
    platform_kind(kind) || kind.starts_with(&namespace_of(slug))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A guest must not be able to climb out of its namespace. Every one of
    /// these is a way to make the final kind mean something other than "this
    /// app said so".
    #[test]
    fn guest_kind_refuses_escapes() {
        let refused = [
            ("empty", String::new()),
            ("leading dot", ".journal.entry.created".into()),
            ("leading dash", "-x".into()),
            ("leading under", "_x".into()),
            ("upper case", "Journal".into()),
            ("space", "entry created".into()),
            ("newline", "entry\ncreated".into()),
            ("carriage return", "entry\rcreated".into()),
            ("slash", "../storage.insert".into()),
            ("colon", "a:b".into()),
            ("null byte", "a\0b".into()),
            ("unicode look-alike", "j\u{043e}urnal".into()),
            ("too long", "a".repeat(97)),
        ];
        for (name, kind) in refused {
            assert!(
                guest_kind(&kind).is_err(),
                "{name}: guest_kind({kind:?}) accepted"
            );
        }
        for kind in ["created", "entry.created", "a", "a-b_c.d", "x9"] {
            assert!(guest_kind(kind).is_ok(), "guest_kind({kind:?}) refused");
        }
    }

    /// The whole point of the prefix: a guest asking for a platform kind gets
    /// its own namespace, not the platform's.
    #[test]
    fn namespace_makes_platform_kinds_unforgeable() {
        let got = format!("{}journal.entry.created", namespace_of("evil"));
        assert!(!platform_kind(&got), "{got:?} reads as a platform kind");
        assert_ne!(got, "journal.entry.created");
        assert!(visible_to(&got, "evil"), "an app cannot see its own event");
        assert!(
            !visible_to(&got, "journal"),
            "app journal can see app evil's event"
        );
    }

    /// Own plus platform, and nothing else.
    #[test]
    fn visible_to_is_own_plus_platform() {
        let cases = [
            (
                "own event",
                format!("{}entry.created", namespace_of("journal")),
                "journal",
                true,
            ),
            (
                "platform event",
                "journal.entry.created".to_string(),
                "journal",
                true,
            ),
            (
                "platform event, other app",
                "storage.insert".to_string(),
                "notes",
                true,
            ),
            (
                "another app's event",
                format!("{}entry.created", namespace_of("notes")),
                "journal",
                false,
            ),
            (
                "prefix collision",
                format!("{}x", namespace_of("journal-evil")),
                "journal",
                false,
            ),
        ];
        for (name, kind, slug, want) in cases {
            assert_eq!(visible_to(&kind, slug), want, "{name}");
        }
    }

    /// The namespaced kind still satisfies the writer's own shape check, or a
    /// legal guest kind becomes a runtime error at append instead of a clean
    /// rejection here. And a 27-character slug leaves the namespaced kind
    /// inside the writer's 128-character limit.
    #[test]
    fn namespaced_kind_satisfies_valid_event_kind() {
        for guest in ["created", "entry.created", "a-b_c.d", "x9"] {
            let kind = format!("{}{guest}", namespace_of("journal"));
            assert!(valid_event_kind(&kind).is_ok(), "{kind:?}");
        }
        let kind = format!("{}x", namespace_of(&"a".repeat(27)));
        assert!(
            valid_event_kind(&kind).is_ok(),
            "a 27-char slug should fit: {kind:?}"
        );
    }
}
