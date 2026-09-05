//! The platform's tool surface.
//!
//! # The one property this crate exists to hold
//!
//! **Everything tools/list shows, tools/call accepts. Everything tools/call
//! accepts, tools/list shows.**
//!
//! The dangerous half is a tool that appears and then refuses. An AI reads a
//! tool list as a menu of things it may do; a tool that is visible and denied
//! teaches it that denials are noise to retry through, and an AI that has
//! learned to treat denials as noise is one that will keep pushing on the
//! denials that matter. Listing something you cannot call is worse than not
//! listing it.
//!
//! The way that guarantee is kept is not care, it is arithmetic: **both paths
//! ask the same predicate the same question.** `list_tools` and `call_tool`
//! both call the guard's `tool_reason` with the same install and the same tool
//! name. There is no second filter, no visibility flag, no "hidden" that means
//! "denied". Two predicates that agree today are two predicates, and the
//! failure mode of two predicates is exactly the one above.
//!
//! Manifest-level `hidden` is not an exception. It removes a tool from the
//! surface before either path sees it, so it is invisible AND uncallable
//! together ... which is the same guarantee, not a violation of it.

use std::sync::Arc;

use async_trait::async_trait;
use hive_identity::Credential;
use hive_manifest::{Impl, Op, Surface, qualified_tool_name, split_tool_name};
use hive_trust::Level;
use serde::Serialize;
use uuid::Uuid;

/// One entry in tools/list, as an MCP client sees it.
#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct Tool {
    /// Qualified by app, so tools/call can route by prefix and two installs
    /// cannot both offer `list`.
    pub name: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub description: String,
    #[serde(rename = "inputSchema", skip_serializing_if = "Option::is_none")]
    pub input_schema: Option<serde_json::Map<String, serde_json::Value>>,
}

/// One installed app in some actor's scope, with its derived surface.
#[derive(Clone, Debug)]
pub struct Install {
    pub id: Uuid,
    pub app: String,
    pub surface: Surface,
}

#[derive(Debug, thiserror::Error)]
pub enum McpError {
    /// A name no install in the actor's scope offers. It is deliberately the
    /// same answer as "you may not call that": distinguishing them tells an
    /// unauthorized caller which tools exist.
    #[error("mcp: no such tool: {0:?}")]
    UnknownTool(String),
    /// A tool name that is not app-qualified.
    #[error("mcp: tool name is not qualified: {0:?}")]
    MalformedName(String),
    #[error(transparent)]
    Credential(#[from] hive_identity::IncompleteCredential),
    #[error("mcp: {0}")]
    Installs(String),
    #[error("mcp: authorize {0}: {1}")]
    Guard(String, String),
    #[error("mcp: dispatch {0}: {1}")]
    Dispatch(String, String),
}

/// Yields the ACTIVE installs an actor could be offered tools from.
///
/// "Could be offered" rather than "may call": this is the candidate set, and
/// every candidate still goes through the predicate. A source that pre-filtered
/// by permission would be a second enforcement point, and the first place the
/// two would disagree is the bug this crate exists to prevent.
#[async_trait]
pub trait Installs: Send + Sync {
    async fn active_installs(&self, cred: &Credential) -> Result<Vec<Install>, String>;
}

/// Decides whether an actor may call one tool of one install.
///
/// One method, used by both paths, deliberately. A trait with a cheap "can see"
/// and an expensive "can call" would be an invitation to use the cheap one for
/// listing, and the two would drift.
///
/// It also carries the audit obligation: an access allowed only by an override
/// writes an audit row inside this call (D18.2), which is why listing audits
/// too. Listing a tool you can only see through an override IS an override
/// access, and making it free would put the obligation back on one call site.
#[async_trait]
pub trait Guard: Send + Sync {
    async fn tool_reason(
        &self,
        cred: &Credential,
        install_id: Uuid,
        tool: &str,
    ) -> Result<bool, String>;
}

/// One invocation of a guest function.
#[derive(Clone, Debug)]
pub struct GuestCall {
    pub install: Install,
    pub cred: Credential,
    pub function: String,
    pub input: Vec<u8>,
}

/// One generated operation.
#[derive(Clone, Debug)]
pub struct CrudCall {
    pub install: Install,
    pub cred: Credential,
    pub collection: String,
    pub op: Op,
    pub input: Vec<u8>,
}

/// What a tool produced.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ToolResult {
    /// Raw JSON.
    pub output: Vec<u8>,
    /// Rides out with the result, because an MCP client is frequently an AI and
    /// this is the value that decides whether the content may reach instruction
    /// position (invariant 9).
    pub trust: Level,
    /// What first made the invocation untrusted, so "why is this untrusted" is
    /// answerable without reading a log.
    pub tainted_by: String,
}

/// Runs a tool once the guard has said yes.
///
/// It never decides permission. Handing it a credential and letting it check
/// would be invariant 11's mistake: a check that takes as an argument the fact
/// it is deciding about.
#[async_trait]
pub trait Dispatcher: Send + Sync {
    /// Invokes an exported guest function.
    async fn call_guest(&self, call: GuestCall) -> Result<ToolResult, String>;
    /// Runs a generated operation host-side, with no guest involved.
    async fn call_crud(&self, call: CrudCall) -> Result<ToolResult, String>;
}

/// Answers tools/list and tools/call for one platform. All three collaborators
/// are required by construction: a server with no guard would be a server that
/// lists everything, which is the failure this crate is about.
pub struct Server {
    installs: Arc<dyn Installs>,
    guard: Arc<dyn Guard>,
    dispatcher: Arc<dyn Dispatcher>,
}

impl Server {
    pub fn new(
        installs: Arc<dyn Installs>,
        guard: Arc<dyn Guard>,
        dispatcher: Arc<dyn Dispatcher>,
    ) -> Server {
        Server {
            installs,
            guard,
            dispatcher,
        }
    }

    /// The union over installs in the connecting actor's scope, filtered by the
    /// same predicate tools/call uses (D2.1). Sorted, so a client diffing two
    /// listings sees real changes rather than map order.
    pub async fn list_tools(&self, cred: &Credential) -> Result<Vec<Tool>, McpError> {
        cred.validate()?;
        let installs = self
            .installs
            .active_installs(cred)
            .await
            .map_err(McpError::Installs)?;
        let mut out = Vec::new();
        for inst in &installs {
            for tool in &inst.surface.tools {
                let ok = self
                    .guard
                    .tool_reason(cred, inst.id, &tool.name)
                    .await
                    .map_err(|e| McpError::Guard(format!("{}.{}", inst.app, tool.name), e))?;
                if !ok {
                    continue;
                }
                out.push(Tool {
                    name: qualified_tool_name(&inst.app, &tool.name),
                    description: tool.description.clone(),
                    input_schema: tool.input_schema.clone(),
                });
            }
        }
        out.sort_by(|a, b| a.name.cmp(&b.name));
        Ok(out)
    }

    /// Routes a qualified name into a guest or a generated operation.
    ///
    /// The lookup and the permission check are the same two steps `list_tools`
    /// takes, in the same order, against the same predicate. That is what makes
    /// the two agree, and it is why this does not consult a cached listing: a
    /// listing is a snapshot, and a grant revoked between listing and calling
    /// has to bite.
    pub async fn call_tool(
        &self,
        cred: &Credential,
        name: &str,
        input: Vec<u8>,
    ) -> Result<ToolResult, McpError> {
        cred.validate()?;
        let (app, tool) =
            split_tool_name(name).ok_or_else(|| McpError::MalformedName(name.to_string()))?;
        let installs = self
            .installs
            .active_installs(cred)
            .await
            .map_err(McpError::Installs)?;
        for inst in installs {
            if inst.app != app {
                continue;
            }
            let Some(t) = inst.surface.tools.iter().find(|t| t.name == tool).cloned() else {
                continue;
            };
            let allowed = self
                .guard
                .tool_reason(cred, inst.id, &t.name)
                .await
                .map_err(|e| McpError::Guard(name.to_string(), e))?;
            if !allowed {
                // Same error as "no such tool", on purpose. Telling an
                // unauthorized caller that a tool exists is an existence
                // oracle, and the tool list they can see already tells them
                // everything they are allowed to know.
                return Err(McpError::UnknownTool(name.to_string()));
            }
            return match t.r#impl {
                Impl::GeneratedCrud => self
                    .dispatcher
                    .call_crud(CrudCall {
                        install: inst,
                        cred: *cred,
                        collection: t.collection.clone(),
                        op: t.op.unwrap_or(Op::List),
                        input,
                    })
                    .await
                    .map_err(|e| McpError::Dispatch(name.to_string(), e)),
                Impl::Guest => self
                    .dispatcher
                    .call_guest(GuestCall {
                        install: inst,
                        cred: *cred,
                        function: t.function.clone(),
                        input,
                    })
                    .await
                    .map_err(|e| McpError::Dispatch(name.to_string(), e)),
            };
        }
        Err(McpError::UnknownTool(name.to_string()))
    }
}
