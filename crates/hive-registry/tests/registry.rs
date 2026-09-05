//! Ported from registry_test.go, evidence_test.go and module_test.go.

use std::sync::Arc;

use hive_manifest::{Collection, Function, Kind, Manifest, Storage, ToolDef};
use hive_registry::{REACTOR_INIT, RegistryError, prepare};
use hive_wasmhost::{
    BytesSource, Capability, CapabilitySet, Config, Deps, Exports, Host, Module, hash_module,
};

const HELLO: &[u8] = include_bytes!("../../hive-wasmhost/testdata/hello.wasm");

fn journalish() -> Manifest {
    Manifest {
        kind: Some(Kind::App),
        name: "journal".into(),
        version: 1,
        storage: Storage {
            collections: vec![
                Collection {
                    name: "entries".into(),
                    ..Default::default()
                },
                Collection {
                    name: "drafts".into(),
                    crud: true,
                    ..Default::default()
                },
            ],
        },
        functions: vec![
            Function {
                name: "add_entry".into(),
                doc: String::new(),
            },
            Function {
                name: "search".into(),
                doc: String::new(),
            },
        ],
        tools: vec![ToolDef {
            name: "journal.add".into(),
            function: "add_entry".into(),
            ..Default::default()
        }],
        ..Default::default()
    }
}

/// The D24 case: every collection generated, no functions, no wasm.
fn crud_only() -> Manifest {
    Manifest {
        kind: Some(Kind::App),
        name: "bookmarks".into(),
        version: 1,
        storage: Storage {
            collections: vec![Collection {
                name: "links".into(),
                crud: true,
                ..Default::default()
            }],
        },
        ..Default::default()
    }
}

async fn host() -> Host {
    Host::new(Config::default(), Deps::default())
        .await
        .expect("host")
}

/// Produces the evidence these tests check manifests against.
///
/// It goes through the real seam: encode a module, compile it on a real host,
/// read back what the host says it exports. There is deliberately no shorter
/// path. `Exports` has a private hash precisely so a caller cannot pair one
/// module's name list with another module's address, and a test-only
/// constructor to save these few milliseconds would hand that back to every
/// caller in the process.
async fn test_exports(h: &Host, names: &[&str]) -> Exports {
    let wasm = module_exporting(names);
    let hash = hash_module(&wasm);
    h.module_exports(
        &Module {
            hash,
            app: "fixture".into(),
            version: "0.0.1".into(),
            memory_pages: 1,
            ..Default::default()
        },
        Arc::new(BytesSource::new(wasm)),
    )
    .await
    .expect("module_exports")
}

/// Hand-encodes the smallest valid wasm module that exports a memory and the
/// named functions, each a no-op.
///
/// Hand-encoded rather than built with a toolchain because the interesting
/// inputs here are export NAMES ("declares add_entry, exports add_entrie") and
/// checking a rename into the build pipeline is a minute per case. The encoder
/// is boring on purpose: one type, N functions of it, one memory, the exports,
/// N empty bodies.
fn module_exporting(names: &[&str]) -> Vec<u8> {
    const SEC_TYPE: u8 = 1;
    const SEC_FUNC: u8 = 3;
    const SEC_MEMORY: u8 = 5;
    const SEC_EXPORT: u8 = 7;
    const SEC_CODE: u8 = 10;
    const EXPORT_FUNC: u8 = 0x00;
    const EXPORT_MEMORY: u8 = 0x02;

    let mut m = vec![0x00, b'a', b's', b'm', 0x01, 0x00, 0x00, 0x00];
    // One type: () -> ().
    m.extend(section(
        SEC_TYPE,
        &[uleb(1), vec![0x60, 0x00, 0x00]].concat(),
    ));
    // N functions, all of type 0.
    let mut funcs = uleb(names.len() as u32);
    funcs.extend(std::iter::repeat_n(0x00, names.len()));
    m.extend(section(SEC_FUNC, &funcs));
    // One memory, minimum one page. check_module refuses a module without
    // one, because the ABI is copies in and out of guest memory.
    m.extend(section(SEC_MEMORY, &[uleb(1), vec![0x00, 0x01]].concat()));
    let mut exports = uleb(names.len() as u32 + 1);
    exports.extend(name("memory"));
    exports.extend([EXPORT_MEMORY, 0x00]);
    for (i, n) in names.iter().enumerate() {
        exports.extend(name(n));
        exports.push(EXPORT_FUNC);
        exports.extend(uleb(i as u32));
    }
    m.extend(section(SEC_EXPORT, &exports));
    // Bodies: no locals, immediate `end`.
    let mut code = uleb(names.len() as u32);
    for _ in names {
        let body = [0x00, 0x0b];
        code.extend(uleb(body.len() as u32));
        code.extend(body);
    }
    m.extend(section(SEC_CODE, &code));
    m
}

fn section(id: u8, body: &[u8]) -> Vec<u8> {
    [vec![id], uleb(body.len() as u32), body.to_vec()].concat()
}

fn name(s: &str) -> Vec<u8> {
    [uleb(s.len() as u32), s.as_bytes().to_vec()].concat()
}

fn uleb(mut v: u32) -> Vec<u8> {
    let mut out = Vec::new();
    loop {
        let mut b = (v & 0x7f) as u8;
        v >>= 7;
        if v != 0 {
            b |= 0x80;
        }
        out.push(b);
        if v == 0 {
            return out;
        }
    }
}

// --- the fixture itself ----------------------------------------------------------

/// A fixture that quietly produces something other than what it says would
/// make every test below pass for the wrong reason.
#[tokio::test]
async fn the_fixture_encoder_produces_what_it_claims() {
    let h = host().await;
    let exports = test_exports(&h, &[REACTOR_INIT, "add_entry", "search"]).await;
    for want in [REACTOR_INIT, "add_entry", "search"] {
        assert!(
            exports.has(want),
            "the encoded module does not export {want:?}; names = {:?}",
            exports.names()
        );
    }
    assert!(
        !exports.has("never_encoded"),
        "the fixture reports an export it was never given"
    );
    assert!(
        !exports.module_hash().is_empty(),
        "evidence with no module hash is not evidence"
    );
    h.close().await;
}

/// Exports must be bound to the bytes they were read from, or the registry's
/// whole premise (a claim meeting its evidence) is a pair of parameters a
/// caller chose independently.
#[tokio::test]
async fn exports_carry_the_hash_of_their_own_module() {
    let h = host().await;
    let one = test_exports(&h, &[REACTOR_INIT, "add_entry"]).await;
    let two = test_exports(&h, &[REACTOR_INIT, "add_entry", "search"]).await;
    assert_ne!(
        one.module_hash(),
        two.module_hash(),
        "two different modules hashed the same"
    );
    assert_eq!(
        one.module_hash(),
        hash_module(&module_exporting(&[REACTOR_INIT, "add_entry"]))
    );
    h.close().await;
}

// --- prepare ---------------------------------------------------------------------

#[tokio::test]
async fn prepare_accepts_a_matching_module() {
    let h = host().await;
    let p = prepare(
        &journalish(),
        &test_exports(&h, &[REACTOR_INIT, "add_entry", "search", "some_helper"]).await,
    )
    .expect("prepare");
    assert!(!p.surface_hash.is_empty(), "no surface hash");
    assert!(
        p.needs_module(),
        "an app with functions reported needing no module"
    );
    let spec = p.install_spec("user", "alice").expect("install_spec");
    assert!(
        spec.schema.schema.starts_with("app_journal_"),
        "schema = {:?}",
        spec.schema.schema
    );
    h.close().await;
}

/// A manifest is a claim and the module is the fact. Deciding this at install
/// is the difference between a bad manifest and a failure on somebody's first
/// call.
#[tokio::test]
async fn prepare_refuses_a_missing_export() {
    let h = host().await;
    let err = prepare(
        &journalish(),
        &test_exports(&h, &[REACTOR_INIT, "add_entry"]).await,
    )
    .expect_err("prepared");
    assert!(
        matches!(err, RegistryError::MissingExport { .. }),
        "err = {err}"
    );
    // The error has to name the missing one AND what is there, because the
    // fix is usually one rename away and a bare refusal is a guess.
    let text = err.to_string();
    assert!(
        text.contains("search"),
        "does not name the missing function: {text}"
    );
    assert!(
        text.contains("add_entry"),
        "does not list what the module exports: {text}"
    );
    h.close().await;
}

/// Extra exports are fine: that is how a guest keeps helpers.
#[tokio::test]
async fn prepare_allows_extra_exports() {
    let h = host().await;
    prepare(
        &journalish(),
        &test_exports(
            &h,
            &[REACTOR_INIT, "add_entry", "search", "helper", "another"],
        )
        .await,
    )
    .expect("extra exports were refused");
    h.close().await;
}

/// A guest that exports _start is a command, not a reactor.
#[tokio::test]
async fn prepare_refuses_a_non_reactor() {
    let h = host().await;
    let err = prepare(
        &journalish(),
        &test_exports(&h, &["_start", "add_entry", "search"]).await,
    )
    .expect_err("prepared");
    assert!(
        matches!(err, RegistryError::NotReactor { .. }),
        "err = {err}"
    );
    assert!(
        err.to_string().contains("reactor"),
        "the error should say how to fix it: {err}"
    );
    h.close().await;
}

/// D24: an app whose collections are all generated CRUD needs no wasm at all.
#[test]
fn crud_only_app_needs_no_module() {
    let p = prepare(&crud_only(), &Exports::none()).expect("prepare");
    assert!(
        !p.needs_module(),
        "a manifest-only app reported needing a module"
    );
    assert!(
        p.module_hash.is_empty(),
        "module hash = {:?}",
        p.module_hash
    );
    // And it still has a usable surface, or the case is pointless.
    assert_eq!(p.surface.tools.len(), 5, "want the five generated tools");
    for tool in &p.surface.tools {
        assert_eq!(
            tool.r#impl,
            hive_manifest::Impl::GeneratedCrud,
            "{} runs on a guest that does not exist",
            tool.name
        );
    }
}

/// Declaring functions with no module is the inverse mistake and fails loudly.
#[test]
fn prepare_refuses_functions_with_no_module() {
    let err = prepare(&journalish(), &Exports::none()).expect_err("prepared");
    assert!(matches!(err, RegistryError::NoModule { .. }), "err = {err}");
}

/// The surface hash has to move when the surface moves and not otherwise.
#[tokio::test]
async fn surface_hash_tracks_the_surface() {
    let h = host().await;
    let base = prepare(
        &journalish(),
        &test_exports(&h, &[REACTOR_INIT, "add_entry", "search"]).await,
    )
    .unwrap();
    let again = prepare(
        &journalish(),
        &test_exports(&h, &[REACTOR_INIT, "add_entry", "search"]).await,
    )
    .unwrap();
    assert_eq!(
        base.surface_hash, again.surface_hash,
        "two preparations of one manifest disagree"
    );

    // A DIFFERENT module with the same surface hashes the same: it is the
    // SURFACE address, not the build's.
    let other = prepare(
        &journalish(),
        &test_exports(&h, &[REACTOR_INIT, "add_entry", "search", "a_helper"]).await,
    )
    .unwrap();
    assert_ne!(
        other.module_hash, base.module_hash,
        "the two fixtures are the same module"
    );
    assert_eq!(
        other.surface_hash, base.surface_hash,
        "the surface hash moved when only the module changed"
    );

    // Adding a tool changes it.
    let mut m = journalish();
    m.tools.push(ToolDef {
        name: "journal.find".into(),
        function: "search".into(),
        ..Default::default()
    });
    let changed = prepare(
        &m,
        &test_exports(&h, &[REACTOR_INIT, "add_entry", "search"]).await,
    )
    .unwrap();
    assert_ne!(
        changed.surface_hash, base.surface_hash,
        "the surface hash did not move when a tool was added"
    );
    h.close().await;
}

/// An invalid manifest never reaches the module check.
#[tokio::test]
async fn prepare_validates_before_checking_the_module() {
    let h = host().await;
    let mut m = journalish();
    m.name = "Journal".into(); // uppercase: refused by validate
    let err =
        prepare(&m, &test_exports(&h, &[]).await).expect_err("an invalid manifest was prepared");
    assert!(
        matches!(err, RegistryError::Manifest(_)),
        "reported a module problem for a manifest problem: {err}"
    );
    h.close().await;
}

/// A surface hash without the deriver that produced it is a number nobody can
/// interpret.
#[tokio::test]
async fn prepared_carries_the_deriver() {
    let h = host().await;
    let p = prepare(
        &journalish(),
        &test_exports(&h, &[REACTOR_INIT, "add_entry", "search"]).await,
    )
    .unwrap();
    assert_eq!(p.derive_version, hive_manifest::DERIVE_VERSION);
    assert!(!p.surface_hash.is_empty() && p.derive_version != 0);
    h.close().await;
}

/// Two owners installing the same app get separate schemas, and therefore
/// separate documents (invariant 14, the schema-name case).
#[test]
fn install_spec_is_scoped_to_the_owner() {
    let p = prepare(&crud_only(), &Exports::none()).unwrap();
    let alice = p.install_spec("user", "alice-id").unwrap();
    let bob = p.install_spec("user", "bob-id").unwrap();
    assert_ne!(
        alice.schema.schema, bob.schema.schema,
        "two owners share a schema"
    );
    // And it is stable, or re-installing would land somewhere new every time.
    let again = p.install_spec("user", "alice-id").unwrap();
    assert_eq!(again.schema.schema, alice.schema.schema);
}

// --- against the real reference guest -------------------------------------------

fn hello_module(caps: &[Capability]) -> Module {
    Module {
        hash: hash_module(HELLO),
        app: "hello".into(),
        version: "0.1.0".into(),
        memory_pages: 256,
        capabilities: CapabilitySet::new(caps),
        ..Default::default()
    }
}

#[tokio::test]
async fn prepare_against_the_real_reference_guest() {
    let h = host().await;
    let exports = h
        .module_exports(
            &hello_module(&[Capability::Log, Capability::Storage]),
            Arc::new(BytesSource::new(HELLO)),
        )
        .await
        .expect("module_exports");
    let m = Manifest {
        kind: Some(Kind::App),
        name: "hello".into(),
        version: 1,
        functions: vec![
            Function {
                name: "hello".into(),
                doc: String::new(),
            },
            Function {
                name: "store_query".into(),
                doc: String::new(),
            },
        ],
        tools: vec![ToolDef {
            name: "hello.greet".into(),
            function: "hello".into(),
            ..Default::default()
        }],
        capabilities: vec!["log".into(), "storage".into()],
        ..Default::default()
    };
    let p = prepare(&m, &exports).expect("prepare against the real guest");
    // The hash on the Prepared came from the evidence rather than from a
    // second parameter.
    assert_eq!(p.module_hash, hash_module(HELLO));
    h.close().await;
}

/// A manifest promising something the real guest does not export.
#[tokio::test]
async fn prepare_catches_a_drifted_manifest() {
    let h = host().await;
    let exports = h
        .module_exports(
            &hello_module(&[Capability::Log, Capability::Storage]),
            Arc::new(BytesSource::new(HELLO)),
        )
        .await
        .unwrap();
    let m = Manifest {
        kind: Some(Kind::App),
        name: "hello".into(),
        version: 1,
        functions: vec![
            Function {
                name: "hello".into(),
                doc: String::new(),
            },
            Function {
                name: "renamed_last_week".into(),
                doc: String::new(),
            },
        ],
        ..Default::default()
    };
    let err = prepare(&m, &exports).expect_err("prepared");
    assert!(
        matches!(err, RegistryError::MissingExport { .. }),
        "err = {err}"
    );
    h.close().await;
}

/// module_exports runs the link checks too, so a module that could never be
/// instantiated is refused at install rather than on a first call.
#[tokio::test]
async fn module_exports_refuses_an_undeclared_capability() {
    let h = host().await;
    // The reference guest imports hive_storage; this grants only log.
    let res = h
        .module_exports(
            &hello_module(&[Capability::Log]),
            Arc::new(BytesSource::new(HELLO)),
        )
        .await;
    assert!(
        res.is_err(),
        "a module importing an ungranted capability was accepted at install"
    );
    h.close().await;
}

/// The reference guest is a reactor, which is what the whole build template is
/// arranged to produce. If this fails, the toolchain changed underneath us.
#[tokio::test]
async fn reference_guest_is_a_reactor() {
    let h = host().await;
    let exports = h
        .module_exports(
            &hello_module(&[Capability::Log, Capability::Storage]),
            Arc::new(BytesSource::new(HELLO)),
        )
        .await
        .unwrap();
    assert!(
        exports.has("_initialize"),
        "the reference guest exports no _initialize; it is not a reactor"
    );
    assert!(
        !exports.has("_start"),
        "the reference guest exports _start; a reactor build should prevent that"
    );
    h.close().await;
}
