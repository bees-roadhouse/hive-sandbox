//! The manifest tests, ported from internal/manifest. Each names the Go test it
//! came from where the name is not the same words.

use hive_manifest::*;
use serde_json::json;

/// The shape the standard set actually has: one hand-written collection whose
/// writes fan out, one generated one.
fn journalish() -> Manifest {
    Manifest {
        kind: Some(Kind::App),
        name: "journal".into(),
        version: 1,
        storage: Storage {
            collections: vec![
                Collection {
                    name: "entries".into(),
                    crud: false,
                    indexes: vec![],
                },
                Collection {
                    name: "drafts".into(),
                    crud: true,
                    indexes: vec![],
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
        tools: vec![
            ToolDef {
                name: "journal.add".into(),
                function: "add_entry".into(),
                ..Default::default()
            },
            ToolDef {
                name: "journal.search".into(),
                function: "search".into(),
                ..Default::default()
            },
        ],
        routes: vec![
            RouteDef {
                method: "POST".into(),
                path: "/entries".into(),
                function: "add_entry".into(),
                hidden: false,
            },
            RouteDef {
                method: "GET".into(),
                path: "/search".into(),
                function: "search".into(),
                hidden: false,
            },
        ],
        subscriptions: vec![],
        // specifically no egress; memory is never the outbound leg
        capabilities: vec![],
    }
}

fn expect_kind(res: Result<(), ValidationErrors>, kind: ErrorKind) {
    match res {
        Err(e) if e.is(kind) => {}
        other => panic!("err = {other:?}, want {kind:?}"),
    }
}

#[test]
fn validate_accepts_the_standard_shape() {
    journalish().validate().expect("validate");
}

#[test]
fn derive_generates_crud_only_for_declared_collections() {
    let s = journalish().derive();
    let want = [
        "drafts.list",
        "drafts.get",
        "drafts.create",
        "drafts.update",
        "drafts.delete",
        "journal.add",
        "journal.search",
    ];
    let got: Vec<&str> = s.tools.iter().map(|t| t.name.as_str()).collect();
    for name in want {
        assert!(got.contains(&name), "missing tool {name:?}");
    }
    for name in &got {
        assert!(want.contains(name), "unexpected tool {name:?}");
    }
    // entries declared crud: false, so nothing was generated for it.
    for t in &s.tools {
        assert!(
            !t.name.starts_with("entries."),
            "generated {:?} for a collection that declared crud: false",
            t.name
        );
    }
}

/// Generated CRUD runs host-side with no guest involved, which is what lets an
/// app that is all CRUD ship without a wasm module at all.
#[test]
fn generated_crud_has_no_guest_function() {
    for t in journalish().derive().tools {
        if !t.name.starts_with("drafts.") {
            continue;
        }
        assert_eq!(t.r#impl, Impl::GeneratedCrud, "{}: impl", t.name);
        assert!(
            t.function.is_empty(),
            "{}: names guest function {:?}",
            t.name,
            t.function
        );
        assert_eq!(t.collection, "drafts", "{}: collection", t.name);
    }
}

/// D16.4: the manifest can still rename, reshape or hide any generated
/// operation. Overriding one must not cost the other four.
#[test]
fn manifest_overrides_one_generated_tool() {
    let mut m = journalish();
    m.functions.push(Function {
        name: "create_draft".into(),
        doc: String::new(),
    });
    m.tools.push(ToolDef {
        name: "drafts.create".into(),
        function: "create_draft".into(),
        description: "Create a draft, with the app's own validation.".into(),
        ..Default::default()
    });
    m.validate().expect("validate");

    let s = m.derive();
    let created = s.tools.iter().find(|t| t.name == "drafts.create");
    let listed = s.tools.iter().find(|t| t.name == "drafts.list");
    let (created, listed) = match (created, listed) {
        (Some(c), Some(l)) => (c, l),
        _ => panic!("override removed tools it should not have"),
    };
    assert_eq!(created.r#impl, Impl::Guest);
    assert_eq!(created.function, "create_draft");
    assert_eq!(
        listed.r#impl,
        Impl::GeneratedCrud,
        "overriding create cost the other four"
    );
}

#[test]
fn hidden_tool_leaves_the_surface() {
    let mut m = journalish();
    m.tools.push(ToolDef {
        name: "drafts.delete".into(),
        hidden: true,
        ..Default::default()
    });
    m.validate().expect("validate");
    assert!(
        !m.derive().tools.iter().any(|t| t.name == "drafts.delete"),
        "a hidden tool is still in the surface"
    );
}

/// A generated name cannot be taken by accident, only overridden on purpose.
#[test]
fn generated_name_cannot_be_shadowed_without_a_function() {
    let mut m = journalish();
    m.tools.push(ToolDef {
        name: "drafts.list".into(),
        ..Default::default()
    });
    expect_kind(m.validate(), ErrorKind::ReservedName);
}

/// The tool tier is a contract, not a convention (D10.3): the host's ability to
/// skip provisioning depends on it holding.
#[test]
fn tool_tier_owns_no_data() {
    let cases: Vec<(&str, Box<dyn Fn(&mut Manifest)>)> = vec![
        (
            "storage",
            Box::new(|m| {
                m.storage.collections = vec![Collection {
                    name: "stuff".into(),
                    ..Default::default()
                }]
            }),
        ),
        (
            "routes",
            Box::new(|m| {
                m.routes = vec![RouteDef {
                    method: "GET".into(),
                    path: "/x".into(),
                    function: "run".into(),
                    hidden: false,
                }]
            }),
        ),
        (
            "subscriptions",
            Box::new(|m| {
                m.subscriptions = vec![Subscription {
                    kind: "entry.created".into(),
                }]
            }),
        ),
        (
            "two functions",
            Box::new(|m| {
                m.functions.push(Function {
                    name: "other".into(),
                    doc: String::new(),
                })
            }),
        ),
    ];
    for (name, mutate) in cases {
        let mut m = Manifest {
            kind: Some(Kind::Tool),
            name: "extract".into(),
            version: 1,
            functions: vec![Function {
                name: "run".into(),
                doc: String::new(),
            }],
            ..Default::default()
        };
        mutate(&mut m);
        match m.validate() {
            Err(e) if e.is(ErrorKind::ToolTier) => {}
            other => panic!("{name}: err = {other:?}, want ToolTier"),
        }
    }
}

#[test]
fn tool_tier_generates_nothing() {
    let m = Manifest {
        kind: Some(Kind::Tool),
        name: "extract".into(),
        version: 1,
        functions: vec![Function {
            name: "run".into(),
            doc: String::new(),
        }],
        tools: vec![ToolDef {
            name: "extract".into(),
            function: "run".into(),
            ..Default::default()
        }],
        ..Default::default()
    };
    m.validate().expect("validate");
    let s = m.derive();
    assert!(
        s.routes.is_empty(),
        "routes = {:?}, want none for a tool",
        s.routes
    );
    assert_eq!(s.tools.len(), 1, "want exactly the one declared");
}

#[test]
fn validate_rejects_dangling_references() {
    let mut m = journalish();
    m.tools.push(ToolDef {
        name: "journal.nope".into(),
        function: "does_not_exist".into(),
        ..Default::default()
    });
    expect_kind(m.validate(), ErrorKind::UnknownFunc);

    let mut m = journalish();
    m.routes.push(RouteDef {
        method: "GET".into(),
        path: "/x".into(),
        function: "does_not_exist".into(),
        hidden: false,
    });
    expect_kind(m.validate(), ErrorKind::UnknownFunc);
}

/// These names become schema names, tool names, URL segments and JSON keys. The
/// set that is safe in all four at once is small, and it is cheaper to refuse
/// at install than to discover in Postgres.
#[test]
fn validate_rejects_unsafe_names() {
    let long = "j".repeat(64);
    for bad in [
        "",
        "Journal",
        "journal-app",
        "1journal",
        "journal.app",
        "drop table",
        "journal;",
        long.as_str(),
    ] {
        let mut m = journalish();
        m.name = bad.to_string();
        match m.validate() {
            Err(e) if e.is(ErrorKind::Name) => {}
            other => panic!("name {bad:?}: err = {other:?}, want Name"),
        }
    }
}

#[test]
fn validate_rejects_bad_routes() {
    for (method, path) in [
        ("TRACE", "/x"),
        ("GET", "no-leading-slash"),
        ("GET", "/../etc"),
    ] {
        let mut m = journalish();
        m.routes.push(RouteDef {
            method: method.into(),
            path: path.into(),
            function: "search".into(),
            hidden: false,
        });
        match m.validate() {
            Err(e) if e.is(ErrorKind::Route) => {}
            other => panic!("route {method} {path}: err = {other:?}, want Route"),
        }
    }
}

/// These validated cleanly and then panicked the Go router at mount, which is a
/// trap for a consumer that does not exist yet. The Rust router's grammar is
/// not the same, but the manifest's contract is the narrower one and stays so.
#[test]
fn validate_rejects_patterns_a_router_refuses() {
    for bad in [
        "/a{b",       // unterminated
        "/{id}/{id}", // repeated wildcard name
        "//double",   // empty segment
        "/./dot",     // relative segment
        "/a{b}",      // wildcard is not the whole segment
        "/{}",        // empty wildcard name
        "/{Id}",      // unusable wildcard name
        "/x}y",       // unmatched brace
        "/{id...}",   // matches everything below it
        "/{a}{b}",    // two wildcards in one segment
    ] {
        let mut m = journalish();
        m.routes.push(RouteDef {
            method: "GET".into(),
            path: bad.into(),
            function: "search".into(),
            hidden: false,
        });
        match m.validate() {
            Err(e) if e.is(ErrorKind::Route) => {}
            other => panic!("path {bad:?}: err = {other:?}, want Route"),
        }
    }
}

/// The other half, and the one that keeps the rule honest: everything validate
/// ACCEPTS must actually mount. In the Go tree this was asserted against a real
/// ServeMux; the Rust API mounts app routes on axum, whose path grammar accepts
/// every one of these (`{id}` is its own capture syntax), so the assertion here
/// is that the accepted set is stable and that each resolves inside its prefix.
#[test]
fn accepted_routes_are_mountable() {
    let good = [
        "/entries",
        "/entries/{id}",
        "/a/b/c",
        "/{id}",
        "/search",
        "/entries/{id}/replies/{reply}",
        "/trailing/",
    ];
    let mut m = journalish();
    m.routes = good
        .iter()
        .map(|p| RouteDef {
            method: "GET".into(),
            path: p.to_string(),
            function: "search".into(),
            hidden: false,
        })
        .collect();
    m.validate().expect("a mountable route was rejected");
    for r in m.derive().routes {
        let full = full_path("journal", &r.path);
        assert!(full.starts_with("/apps/journal/"), "{full}");
        assert!(!full.contains("//"), "{full}");
    }
}

/// Refused on one surface and silently resolved on the other was the whole of
/// finding 3: routes had no duplicate check, so the last declaration won and
/// which one that was depended on file order.
#[test]
fn validate_rejects_duplicate_routes() {
    let mut m = journalish();
    m.routes.push(RouteDef {
        method: "POST".into(),
        path: "/entries".into(),
        function: "search".into(),
        hidden: false,
    });
    expect_kind(m.validate(), ErrorKind::Duplicate);

    // Same path, different method, is not a duplicate.
    let mut m = journalish();
    m.routes.push(RouteDef {
        method: "DELETE".into(),
        path: "/entries".into(),
        function: "search".into(),
        hidden: false,
    });
    m.validate()
        .expect("distinct methods on one path were refused");
}

#[test]
fn validate_rejects_duplicates() {
    let mut m = journalish();
    m.functions.push(Function {
        name: "search".into(),
        doc: String::new(),
    });
    expect_kind(m.validate(), ErrorKind::Duplicate);
}

/// The registry content-addresses what it installs, so two derivations of the
/// same manifest have to be identical. Marshal the whole Surface and compare
/// bytes, over a corpus big enough for the failure it names to be possible.
#[test]
fn derive_is_deterministic() {
    fn build() -> Manifest {
        let mut m = Manifest {
            kind: Some(Kind::App),
            name: "big".into(),
            version: 1,
            ..Default::default()
        };
        for i in 0..40 {
            let name = format!("c{i:02}");
            m.storage.collections.push(Collection {
                name,
                crud: true,
                indexes: vec![format!("btree(f{i:02})"), format!("gin(t{i:02})")],
            });
            let fn_name = format!("fn{i:02}");
            m.functions.push(Function {
                name: fn_name.clone(),
                doc: String::new(),
            });
            let schema = json!({
                "type": "object",
                "properties": {
                    "zeta": {"type": "string"},
                    "beta": {"type": "integer"},
                    "iota": {"type": "boolean"},
                    "nu": {"type": "number"},
                },
            });
            m.tools.push(ToolDef {
                name: format!("h{i:02}.run"),
                function: fn_name.clone(),
                description: "hand written".into(),
                input_schema: Some(schema.as_object().unwrap().clone()),
                hidden: false,
            });
            m.routes.push(RouteDef {
                method: "GET".into(),
                path: format!("/h{i:02}"),
                function: fn_name,
                hidden: false,
            });
            // Declared in reverse, so capability_names has real work to do.
            m.capabilities.push(format!("cap{:02}", 40 - i));
        }
        m
    }

    build().validate().expect("fixture is invalid");
    let first = serde_json::to_vec(&build().derive()).unwrap();
    assert!(
        first.len() >= 10000,
        "fixture is {} bytes; too small to catch what this test claims",
        first.len()
    );
    for i in 0..50 {
        let next = serde_json::to_vec(&build().derive()).unwrap();
        assert_eq!(
            first, next,
            "derivation {i} differs from the first; the surface is not content-addressable"
        );
    }
}

/// Tool names may contain one dot, so qualification has to split on the first.
#[test]
fn tool_name_round_trips() {
    for (app, tool) in [("journal", "add"), ("journal", "drafts.list")] {
        let q = qualified_tool_name(app, tool);
        assert_eq!(split_tool_name(&q), Some((app, tool)), "{q} round-trip");
    }
    assert!(
        split_tool_name("nodot").is_none(),
        "an unqualified name was accepted"
    );
}

/// The host owns the mount prefix, so an app cannot collide with another
/// install. Every escape below is refused by validate, which is the real
/// defence; full_path is concatenation and is documented as not being a
/// boundary ... this asserts the boundary is where the doc says it is.
#[test]
fn mount_path_is_host_owned() {
    for escape in [
        "/../other",
        "/..",
        "/../../etc",
        "/a/../../b",
        "/a/../../../root",
    ] {
        let mut m = journalish();
        m.routes.push(RouteDef {
            method: "GET".into(),
            path: escape.into(),
            function: "search".into(),
            hidden: false,
        });
        match m.validate() {
            Err(e) if e.is(ErrorKind::Route) => {}
            other => panic!("path {escape:?} was accepted: err = {other:?}"),
        }
    }

    // For every path validate ACCEPTS, full_path stays inside the prefix even
    // after the path is normalised.
    let accepted = [
        "/entries",
        "/entries/{id}",
        "/a/b/c",
        "/{id}",
        "/trailing/",
        "/.hidden",
        "/x..y",
    ];
    let mut m = journalish();
    m.routes = accepted
        .iter()
        .map(|p| RouteDef {
            method: "GET".into(),
            path: p.to_string(),
            function: "search".into(),
            hidden: false,
        })
        .collect();
    m.validate().expect("fixture rejected");
    for r in m.derive().routes {
        let full = full_path("journal", &r.path);
        let cleaned = clean_path(&full);
        assert!(
            cleaned.starts_with(&format!("{}/", mount_path("journal"))),
            "accepted route {} resolves to {cleaned:?}, outside {:?}",
            r.path,
            mount_path("journal")
        );
    }

    assert_ne!(
        full_path("journal", "/x"),
        full_path("calendar", "/x"),
        "two apps derived the same mount path"
    );
}

/// path.Clean, enough of it for the assertion above.
fn clean_path(p: &str) -> String {
    let mut out: Vec<&str> = Vec::new();
    for seg in p.split('/') {
        match seg {
            "" | "." => {}
            ".." => {
                out.pop();
            }
            s => out.push(s),
        }
    }
    format!("/{}", out.join("/"))
}

// ---- schema -----------------------------------------------------------------

#[test]
fn parse_index_accepts_the_declared_vocabulary() {
    let cases = [
        (
            "btree(updated_at)",
            IndexMethod::BTree,
            vec!["updated_at"],
            0,
        ),
        ("gin(tags)", IndexMethod::Gin, vec!["tags"], 0),
        ("fts(body)", IndexMethod::Fts, vec!["body"], 0),
        (
            "vector(embedding, 1536)",
            IndexMethod::Vector,
            vec!["embedding"],
            1536,
        ),
        (
            "btree(author.name)",
            IndexMethod::BTree,
            vec!["author", "name"],
            0,
        ),
        (
            "  btree( updated_at )  ",
            IndexMethod::BTree,
            vec!["updated_at"],
            0,
        ),
    ];
    for (decl, method, path, dim) in cases {
        let got = parse_index(decl).unwrap_or_else(|e| panic!("{decl:?}: {e}"));
        assert_eq!(got.method, method, "{decl:?}");
        assert_eq!(got.path, path, "{decl:?}");
        assert_eq!(got.dim, dim, "{decl:?}");
    }
}

/// An index declaration ends up inside CREATE INDEX and a manifest is a file an
/// AI writes. These are the shapes that must not survive parsing.
#[test]
fn parse_index_refuses_everything_else() {
    for decl in [
        "",
        "updated_at",                       // no method
        "hash(updated_at)",                 // unknown method
        "btree()",                          // no path
        "btree(updated_at); DROP TABLE x",  // trailing statement
        "btree(updated_at) -- comment",     // trailing comment
        "btree(\"updated_at\")",            // quoted identifier
        "btree(updated_at, extra)",         // btree takes one path
        "vector(embedding)",                // dimension is not optional
        "vector(embedding, 0)",             // dimension out of range
        "vector(embedding, 99999)",         //
        "vector(embedding, abc)",           // dimension not a number
        "btree(Updated_At)",                // uppercase segment
        "btree(updated-at)",                // hyphen
        "btree(a)); DROP SCHEMA app_x; --", // nested parens
    ] {
        match parse_index(decl) {
            Ok(got) => panic!("{decl:?} parsed to {got:?}; it must be refused"),
            Err(e) => assert_eq!(e.kind, ErrorKind::Index, "{decl:?}: {e}"),
        }
    }
    // This one PARSES, and that is correct: it is a legal document path.
    // Nothing here can reach a catalog, because the path is a path inside the
    // app's own JSON document, not a table reference. Recorded so nobody later
    // "hardens" it into a denylist.
    parse_index("btree(pg_catalog.pg_class)").expect("a legal document path was refused");
}

#[test]
fn index_string_round_trips() {
    for decl in [
        "btree(updated_at)",
        "gin(tags)",
        "fts(body)",
        "vector(embedding, 1536)",
        "btree(author.name)",
    ] {
        let idx = parse_index(decl).unwrap();
        assert_eq!(idx.to_string(), decl);
    }
}

#[test]
fn validate_rejects_bad_indexes() {
    let mut m = journalish();
    m.storage.collections[0].indexes = vec!["btree(entry_date); DROP TABLE x".into()];
    expect_kind(m.validate(), ErrorKind::Index);
}

/// Postgres truncates an over-long identifier rather than rejecting it, so two
/// apps differing only past the limit would quietly share a schema.
#[test]
fn validate_bounds_the_derived_schema_name() {
    let mut m = journalish();
    m.name = "a".repeat(MAX_APP_NAME);
    m.validate().expect("a name that fits was rejected");
    assert!(
        schema_name(&m.name, "user", "an-owner-id").len() <= MAX_IDENTIFIER,
        "schema_name is over the limit"
    );

    m.name = "a".repeat(MAX_APP_NAME + 1);
    let err = m.validate().expect_err("an over-long name was accepted");
    assert!(err.is(ErrorKind::Name), "{err}");
    // The message shows the SHAPE, because the real name depends on an owner a
    // manifest does not know.
    assert!(
        err.to_string().contains("owner"),
        "the error should explain that the owner suffix needs room: {err}"
    );
}

/// The same class as the schema-name bound, one level down: nothing bounded what
/// the applier DERIVES, and Postgres truncates rather than rejecting.
#[test]
fn validate_bounds_derived_collection_names() {
    let fits = format!("c{}", "x".repeat(MAX_COLLECTION_NAME - 1));
    let mut m = journalish();
    m.storage.collections[0].name = fits;
    m.validate()
        .expect("a collection name that fits was rejected");

    let too_long = format!("c{}", "x".repeat(MAX_COLLECTION_NAME));
    let mut m = journalish();
    m.storage.collections[0].name = too_long;
    expect_kind(m.validate(), ErrorKind::Name);
}

/// The budget has to cover every suffix the platform appends, or a derived name
/// truncates onto the table it belongs to.
#[test]
fn derived_suffix_budget_covers_every_suffix() {
    for suffix in DERIVED_SUFFIXES {
        assert!(
            MAX_COLLECTION_NAME + suffix.len() <= MAX_IDENTIFIER,
            "{suffix} does not fit behind a maximal collection name"
        );
    }
    // The longest one uses the whole budget; anything else would be slack
    // that a new, longer suffix could silently eat.
    let longest = DERIVED_SUFFIXES.iter().map(|s| s.len()).max().unwrap();
    assert_eq!(MAX_COLLECTION_NAME + longest, MAX_IDENTIFIER);
}

#[test]
fn schema_name_is_per_owner() {
    let a = schema_name("journal", "user", "11111111-1111-1111-1111-111111111111");
    let b = schema_name("journal", "user", "22222222-2222-2222-2222-222222222222");
    assert_ne!(a, b, "two owners of one app share a schema");
    assert!(a.starts_with("app_journal_"));
    assert_eq!(a.len(), "app_journal_".len() + 8);
}

#[test]
fn manifest_json_keeps_the_shape_the_data_layer_reads() {
    // ResolveActiveInstall reads manifest->'storage'->'collections'->>'name'
    // off the build row, so the wire shape of a stored manifest is part of the
    // schema contract.
    let m = journalish();
    let v: serde_json::Value = serde_json::to_value(&m).unwrap();
    assert_eq!(v["kind"], "app");
    assert_eq!(v["storage"]["collections"][0]["name"], "entries");
    assert_eq!(v["storage"]["collections"][1]["crud"], true);
    assert!(
        v["storage"]["collections"][0].get("crud").is_none(),
        "false is omitted, as omitempty did"
    );
    let back: Manifest = serde_json::from_value(v).unwrap();
    assert_eq!(back, m);
}

// ---- golden ------------------------------------------------------------------

/// A fixture that only reaches the easy path is shape three from CLAUDE.md: the
/// property is real and the assertion is honest, and the interesting branch
/// never runs. So this one has a generated collection, a non-generated one, an
/// override of a generated tool, a hidden generated tool, a route override,
/// capabilities declared out of order, and a multi-key input schema ... because
/// that last is the only thing that makes map key ordering observable at all.
fn golden_manifest() -> Manifest {
    let schema = json!({
        "type": "object",
        "properties": {
            "zeta": {"type": "string"},
            "alpha": {"type": "integer"},
            "omega": {"type": "boolean"},
            "mu": {"type": "number"},
        },
        "required": ["alpha"],
    });
    Manifest {
        kind: Some(Kind::App),
        name: "golden".into(),
        version: 3,
        storage: Storage {
            collections: vec![
                Collection {
                    name: "entries".into(),
                    crud: false,
                    indexes: vec![
                        "btree(entry_date)".into(),
                        "gin(tags)".into(),
                        "fts(body)".into(),
                    ],
                },
                Collection {
                    name: "drafts".into(),
                    crud: true,
                    indexes: vec!["btree(updated_at)".into()],
                },
                Collection {
                    name: "links".into(),
                    crud: true,
                    indexes: vec![],
                },
            ],
        },
        functions: vec![
            Function {
                name: "add_entry".into(),
                doc: "fans out".into(),
            },
            Function {
                name: "search".into(),
                doc: String::new(),
            },
            Function {
                name: "create_draft".into(),
                doc: String::new(),
            },
        ],
        tools: vec![
            ToolDef {
                name: "golden.add".into(),
                function: "add_entry".into(),
                description: "Add an entry.".into(),
                ..Default::default()
            },
            // Overrides a generated one.
            ToolDef {
                name: "drafts.create".into(),
                function: "create_draft".into(),
                ..Default::default()
            },
            // Removes a generated one.
            ToolDef {
                name: "drafts.delete".into(),
                hidden: true,
                ..Default::default()
            },
            ToolDef {
                name: "golden.search".into(),
                function: "search".into(),
                input_schema: Some(schema.as_object().unwrap().clone()),
                ..Default::default()
            },
        ],
        routes: vec![
            RouteDef {
                method: "POST".into(),
                path: "/entries".into(),
                function: "add_entry".into(),
                hidden: false,
            },
            RouteDef {
                method: "GET".into(),
                path: "/entries/{id}".into(),
                function: "search".into(),
                hidden: false,
            },
            // Overrides a generated route.
            RouteDef {
                method: "POST".into(),
                path: "/drafts".into(),
                function: "create_draft".into(),
                hidden: false,
            },
            RouteDef {
                method: "DELETE".into(),
                path: "/drafts/{id}".into(),
                function: String::new(),
                hidden: true,
            },
        ],
        subscriptions: vec![Subscription {
            kind: "entry.created".into(),
        }],
        capabilities: vec!["storage".into(), "log".into(), "kv".into(), "log".into()],
    }
}

/// Pins what DERIVE_VERSION 2 produces.
///
/// It is not here to stop derive changing. It is here so that changing it is a
/// DECISION: this fails, you look, and either the change was unintended or you
/// bump DERIVE_VERSION so that every persisted surface hash stays attributable
/// to the deriver that produced it.
const GOLDEN_HASH: &str = "GOLDEN_PLACEHOLDER";

#[test]
fn derived_surface_is_golden() {
    use sha2::{Digest, Sha256};
    let m = golden_manifest();
    m.validate().expect("the golden fixture is invalid");
    let b = serde_json::to_vec(&m.derive()).unwrap();
    let got = hex::encode(Sha256::digest(&b));
    assert_eq!(
        got,
        GOLDEN_HASH,
        "the derived surface changed.\n\nIf that was deliberate, bump DERIVE_VERSION (currently {DERIVE_VERSION}) and update GOLDEN_HASH. Every surface hash already persisted on a build was produced by the OLD deriver, and without a version bump a promotion reviewer cannot tell \"the app changed\" from \"we changed\".\n\nSurface was:\n{}",
        String::from_utf8_lossy(&b)
    );
}

/// The golden fixture has to actually reach the branches it claims to, or it is
/// a well-written test of the easy path. It protects against coverage
/// SHRINKING, not against it failing to GROW.
#[test]
fn golden_fixture_reaches_every_branch() {
    let s = golden_manifest().derive();
    let mut generated = 0;
    let mut guest = 0;
    let mut saw_override = false;
    let mut saw_schema = false;
    for t in &s.tools {
        match t.r#impl {
            Impl::GeneratedCrud => generated += 1,
            Impl::Guest => guest += 1,
        }
        if t.name == "drafts.create" {
            assert_eq!(t.r#impl, Impl::Guest, "the override branch did not run");
            saw_override = true;
        }
        if t.input_schema.as_ref().is_some_and(|m| !m.is_empty()) {
            saw_schema = true;
        }
        assert_ne!(t.name, "drafts.delete", "the hidden branch did not run");
    }
    assert!(
        generated > 0,
        "no generated tools; the CRUD branch never ran"
    );
    assert!(guest > 0, "no guest tools");
    assert!(saw_override, "no override survived");
    assert!(
        saw_schema,
        "no multi-key input schema; the golden cannot see key ordering"
    );
    assert_eq!(
        s.capabilities.len(),
        3,
        "capabilities = {:?}, want three deduplicated and sorted",
        s.capabilities
    );
    assert!(
        !s.routes
            .iter()
            .any(|r| r.method == "DELETE" && r.path == "/drafts/{id}"),
        "the hidden route branch did not run"
    );
    let drafts_post = s
        .routes
        .iter()
        .find(|r| r.method == "POST" && r.path == "/drafts")
        .unwrap();
    assert_eq!(
        drafts_post.r#impl,
        Impl::Guest,
        "the route override branch did not run"
    );
}
