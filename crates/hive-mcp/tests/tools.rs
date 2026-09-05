//! The tool surface, ported from internal/mcp/tools_test.go.

use std::collections::HashMap;
use std::sync::Arc;

use async_trait::async_trait;
use hive_identity::{Credential, PrincipalKind};
use hive_manifest::*;
use hive_mcp::*;
use hive_trust::Level;
use parking_lot::Mutex;
use uuid::{Uuid, uuid};

const ACTOR_AVA: Uuid = uuid!("11111111-1111-4111-8111-111111111111");
const PRINCIPAL_ALICE: Uuid = uuid!("22222222-2222-4222-8222-222222222222");
const INSTALL_ONE: Uuid = uuid!("33333333-3333-4333-8333-333333333333");
const INSTALL_TWO: Uuid = uuid!("44444444-4444-4444-8444-444444444444");

fn alice() -> Credential {
    Credential::new(ACTOR_AVA, PrincipalKind::User, PRINCIPAL_ALICE)
}

/// A hand-written tool and a generated CRUD collection, so both dispatch
/// branches are reachable.
fn journal_surface() -> Surface {
    let m = Manifest {
        kind: Some(Kind::App),
        name: "journal".into(),
        version: 1,
        storage: Storage {
            collections: vec![Collection {
                name: "drafts".into(),
                crud: true,
                indexes: vec![],
            }],
        },
        functions: vec![Function {
            name: "add_entry".into(),
            doc: String::new(),
        }],
        tools: vec![ToolDef {
            name: "journal.add".into(),
            function: "add_entry".into(),
            ..Default::default()
        }],
        ..Default::default()
    };
    m.validate().expect("fixture invalid");
    m.derive()
}

struct FakeInstalls(Vec<Install>);

#[async_trait]
impl Installs for FakeInstalls {
    async fn active_installs(&self, _: &Credential) -> Result<Vec<Install>, String> {
        Ok(self.0.clone())
    }
}

/// Allows exactly the (install, tool) pairs it was given, and counts how often
/// it was asked. The count is what proves both paths consult it.
struct FakeGuard {
    allow: Mutex<HashMap<String, bool>>,
    asked: Mutex<HashMap<String, usize>>,
}

fn key(install: Uuid, tool: &str) -> String {
    format!("{install}/{tool}")
}

fn guard(allowed: &[String]) -> Arc<FakeGuard> {
    Arc::new(FakeGuard {
        allow: Mutex::new(allowed.iter().map(|a| (a.clone(), true)).collect()),
        asked: Mutex::new(HashMap::new()),
    })
}

#[async_trait]
impl Guard for FakeGuard {
    async fn tool_reason(
        &self,
        _: &Credential,
        install_id: Uuid,
        tool: &str,
    ) -> Result<bool, String> {
        let k = key(install_id, tool);
        *self.asked.lock().entry(k.clone()).or_insert(0) += 1;
        Ok(self.allow.lock().get(&k).copied().unwrap_or(false))
    }
}

#[derive(Default)]
struct FakeDispatcher {
    guest_calls: Mutex<Vec<GuestCall>>,
    crud_calls: Mutex<Vec<CrudCall>>,
}

#[async_trait]
impl Dispatcher for FakeDispatcher {
    async fn call_guest(&self, call: GuestCall) -> Result<ToolResult, String> {
        self.guest_calls.lock().push(call);
        Ok(ToolResult {
            output: br#"{"via":"guest"}"#.to_vec(),
            trust: Level::Trusted,
            tainted_by: String::new(),
        })
    }
    async fn call_crud(&self, call: CrudCall) -> Result<ToolResult, String> {
        self.crud_calls.lock().push(call);
        Ok(ToolResult {
            output: br#"{"via":"crud"}"#.to_vec(),
            trust: Level::Trusted,
            tainted_by: String::new(),
        })
    }
}

fn server_with(installs: Vec<Install>, g: Arc<FakeGuard>) -> (Server, Arc<FakeDispatcher>) {
    let d = Arc::new(FakeDispatcher::default());
    let s = Server::new(Arc::new(FakeInstalls(installs)), g, d.clone());
    (s, d)
}

fn install_one() -> Install {
    Install {
        id: INSTALL_ONE,
        app: "journal".into(),
        surface: journal_surface(),
    }
}

/// THE property. Not "listing filters correctly" and not "calling denies
/// correctly", but that the two agree, over every subset of grants.
#[tokio::test]
async fn list_and_call_agree_on_every_grant_subset() {
    let surface = journal_surface();
    let names: Vec<String> = surface.tools.iter().map(|t| t.name.clone()).collect();
    assert!(
        names.len() >= 6,
        "fixture offers {} tools; too few to exercise subsets",
        names.len()
    );

    for mask in 0..(1usize << names.len()) {
        let allowed: Vec<String> = names
            .iter()
            .enumerate()
            .filter(|(i, _)| mask & (1 << i) != 0)
            .map(|(_, n)| key(INSTALL_ONE, n))
            .collect();
        let (s, _) = server_with(vec![install_one()], guard(&allowed));

        let listed = s.list_tools(&alice()).await.unwrap();
        let in_listing: Vec<String> = listed.into_iter().map(|t| t.name).collect();
        for n in &names {
            let qualified = qualified_tool_name("journal", n);
            let callable = s
                .call_tool(&alice(), &qualified, b"{}".to_vec())
                .await
                .is_ok();
            assert_eq!(
                in_listing.contains(&qualified),
                callable,
                "mask {mask}: {qualified} listed={} callable={callable}; the two disagree",
                in_listing.contains(&qualified)
            );
        }
    }
}

/// Both paths ask the predicate. If either stopped, the agreement test above
/// could still pass by both consulting the same stale cache.
#[tokio::test]
async fn both_paths_consult_the_guard() {
    let g = guard(&[key(INSTALL_ONE, "journal.add")]);
    let (s, _) = server_with(vec![install_one()], g.clone());
    s.list_tools(&alice()).await.unwrap();
    let after_list = *g
        .asked
        .lock()
        .get(&key(INSTALL_ONE, "journal.add"))
        .unwrap_or(&0);
    assert!(after_list > 0, "list_tools did not consult the guard");
    s.call_tool(&alice(), "journal.journal.add", b"{}".to_vec())
        .await
        .unwrap();
    assert!(
        *g.asked
            .lock()
            .get(&key(INSTALL_ONE, "journal.add"))
            .unwrap()
            > after_list,
        "call_tool did not consult the guard; it trusted the listing"
    );
}

/// A grant revoked between listing and calling has to bite.
#[tokio::test]
async fn revocation_between_list_and_call_bites() {
    let k = key(INSTALL_ONE, "journal.add");
    let g = guard(std::slice::from_ref(&k));
    let (s, _) = server_with(vec![install_one()], g.clone());
    let listed = s.list_tools(&alice()).await.unwrap();
    assert!(!listed.is_empty(), "nothing listed");
    g.allow.lock().insert(k, false);
    let err = s
        .call_tool(&alice(), "journal.journal.add", b"{}".to_vec())
        .await
        .err()
        .unwrap();
    assert!(
        matches!(err, McpError::UnknownTool(_)),
        "a revoked grant was served from a stale listing: {err}"
    );
}

/// Denied and non-existent are the same answer. Distinguishing them tells an
/// unauthorized caller which tools exist.
#[tokio::test]
async fn denied_and_unknown_are_indistinguishable() {
    let (s, _) = server_with(vec![install_one()], guard(&[]));
    let denied = s
        .call_tool(&alice(), "journal.journal.add", b"{}".to_vec())
        .await
        .err()
        .unwrap();
    let missing = s
        .call_tool(&alice(), "journal.does_not_exist", b"{}".to_vec())
        .await
        .err()
        .unwrap();
    assert!(matches!(denied, McpError::UnknownTool(_)), "{denied}");
    assert!(matches!(missing, McpError::UnknownTool(_)), "{missing}");
    assert_eq!(
        denied.to_string(),
        missing
            .to_string()
            .replacen("does_not_exist", "journal.add", 1)
    );
}

/// Two installs of different apps cannot see each other's tools, and the
/// qualified name is what keeps them apart.
#[tokio::test]
async fn tools_are_scoped_to_their_install() {
    let two = Install {
        id: INSTALL_TWO,
        app: "diary".into(),
        surface: journal_surface(),
    };
    // Granted on install ONE only.
    let (s, _) = server_with(
        vec![install_one(), two],
        guard(&[key(INSTALL_ONE, "journal.add")]),
    );
    let listed = s.list_tools(&alice()).await.unwrap();
    assert_eq!(listed.len(), 1);
    assert_eq!(listed[0].name, "journal.journal.add");
    // The same tool name on the other install is refused, because the grant
    // is per install rather than per name.
    let err = s
        .call_tool(&alice(), "diary.journal.add", b"{}".to_vec())
        .await
        .err()
        .unwrap();
    assert!(
        matches!(err, McpError::UnknownTool(_)),
        "a grant on one install reached another"
    );
}

/// Generated CRUD dispatches host-side with no guest, which is what lets an app
/// that is all CRUD work without a wasm module (D24).
#[tokio::test]
async fn generated_tools_dispatch_without_a_guest() {
    let (s, d) = server_with(
        vec![install_one()],
        guard(&[
            key(INSTALL_ONE, "drafts.list"),
            key(INSTALL_ONE, "journal.add"),
        ]),
    );
    s.call_tool(&alice(), "journal.drafts.list", b"{}".to_vec())
        .await
        .unwrap();
    {
        let crud = d.crud_calls.lock();
        assert_eq!(crud.len(), 1);
        assert!(
            d.guest_calls.lock().is_empty(),
            "a generated tool reached the guest dispatcher"
        );
        assert_eq!(crud[0].collection, "drafts");
        assert_eq!(crud[0].op, Op::List);
    }
    s.call_tool(&alice(), "journal.journal.add", b"{}".to_vec())
        .await
        .unwrap();
    let guest = d.guest_calls.lock();
    assert_eq!(guest.len(), 1);
    assert_eq!(guest[0].function, "add_entry");
}

/// A hidden tool is absent from both paths together, which is the same
/// guarantee rather than an exception to it.
#[tokio::test]
async fn hidden_tools_are_neither_listed_nor_callable() {
    let m = Manifest {
        kind: Some(Kind::App),
        name: "journal".into(),
        version: 1,
        storage: Storage {
            collections: vec![Collection {
                name: "drafts".into(),
                crud: true,
                indexes: vec![],
            }],
        },
        tools: vec![ToolDef {
            name: "drafts.delete".into(),
            hidden: true,
            ..Default::default()
        }],
        ..Default::default()
    };
    m.validate().expect("fixture invalid");
    let inst = Install {
        id: INSTALL_ONE,
        app: "journal".into(),
        surface: m.derive(),
    };
    // Granted, so only hiding can keep it out.
    let (s, _) = server_with(vec![inst], guard(&[key(INSTALL_ONE, "drafts.delete")]));
    for t in s.list_tools(&alice()).await.unwrap() {
        assert!(
            !t.name.ends_with("drafts.delete"),
            "a hidden tool was listed"
        );
    }
    let err = s
        .call_tool(&alice(), "journal.drafts.delete", b"{}".to_vec())
        .await
        .err()
        .unwrap();
    assert!(
        matches!(err, McpError::UnknownTool(_)),
        "a hidden tool was callable"
    );
}

/// An incomplete credential never reaches the guard, because absence of scope
/// is deny rather than a question to ask.
#[tokio::test]
async fn incomplete_credential_is_refused() {
    let g = guard(&[key(INSTALL_ONE, "journal.add")]);
    let (s, _) = server_with(vec![install_one()], g.clone());
    for cred in [
        Credential::new(Uuid::nil(), PrincipalKind::User, Uuid::nil()),
        Credential::new(ACTOR_AVA, PrincipalKind::User, Uuid::nil()),
        Credential::new(Uuid::nil(), PrincipalKind::User, PRINCIPAL_ALICE),
    ] {
        assert!(
            s.list_tools(&cred).await.is_err(),
            "list_tools accepted an incomplete credential"
        );
        assert!(
            s.call_tool(&cred, "journal.journal.add", Vec::new())
                .await
                .is_err(),
            "call_tool accepted an incomplete credential"
        );
    }
    assert!(
        g.asked.lock().is_empty(),
        "an incomplete credential reached the guard"
    );
}
