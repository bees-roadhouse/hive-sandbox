//! The run record seam: what gets written when a run starts, and where.

use std::collections::HashMap;
use std::time::Duration;

use async_trait::async_trait;
use parking_lot::Mutex;

use crate::spec::{Event, Limits, NetworkMode, RunResult, Runtime};

/// What gets written when a run starts. Everything here is what D12.5 wants on
/// the row: which image, which CLI, which model, which session, and the caps it
/// ran under.
///
/// Note what is NOT here: no actor, no principal, no trust, no step. A store
/// pins those from the credential it was constructed with, and this record must
/// never grow them (invariant 11).
#[derive(Clone, Debug, PartialEq)]
pub struct RunRecord {
    pub run_id: String,
    pub runtime: Runtime,
    pub image_digest: String,
    pub cli_version: String,
    pub model: String,
    pub session_id: String,
    pub network: NetworkMode,
    pub limits: Limits,
    pub deadline: Duration,
    pub started_at: chrono::DateTime<chrono::Utc>,
}

/// A store that was asked about a run it never saw, or refused one it had.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum StoreError {
    #[error("harness: run not found: {0}")]
    RunNotFound(String),
    #[error("harness: run already exists: {0}")]
    RunExists(String),
    #[error("harness: store: {0}")]
    Other(String),
}

/// Persists runs and their output.
///
/// Implementations must tolerate `append_event` being called from the
/// supervisor's drain path: it is on the critical path for a child process's
/// pipe, so a slow store slows the agent and a blocking one hangs it.
#[async_trait]
pub trait RunStore: Send + Sync {
    async fn create_run(&self, rec: RunRecord) -> Result<(), StoreError>;
    async fn append_event(&self, run_id: &str, ev: Event) -> Result<(), StoreError>;
    async fn finish_run(&self, run_id: &str, res: RunResult) -> Result<(), StoreError>;
}

/// One run's full state in a [`MemoryStore`].
#[derive(Clone, Debug)]
pub struct StoredRun {
    pub record: RunRecord,
    pub events: Vec<Event>,
    pub result: Option<RunResult>,
}

impl StoredRun {
    pub fn done(&self) -> bool {
        self.result.is_some()
    }
}

/// Keeps runs in memory. It is the development and test implementation; it is
/// not durable and makes no attempt to be.
#[derive(Default)]
pub struct MemoryStore {
    inner: Mutex<MemoryInner>,
}

#[derive(Default)]
struct MemoryInner {
    runs: HashMap<String, StoredRun>,
    /// Preserves insertion order so tests can assert on it without sorting.
    order: Vec<String>,
}

impl MemoryStore {
    pub fn new() -> Self {
        Self::default()
    }

    /// A copy of one run's state.
    pub fn run(&self, run_id: &str) -> Result<StoredRun, StoreError> {
        self.inner
            .lock()
            .runs
            .get(run_id)
            .cloned()
            .ok_or_else(|| StoreError::RunNotFound(run_id.to_string()))
    }

    /// Every run id in creation order.
    pub fn run_ids(&self) -> Vec<String> {
        self.inner.lock().order.clone()
    }
}

#[async_trait]
impl RunStore for MemoryStore {
    async fn create_run(&self, rec: RunRecord) -> Result<(), StoreError> {
        let mut inner = self.inner.lock();
        if inner.runs.contains_key(&rec.run_id) {
            return Err(StoreError::RunExists(rec.run_id));
        }
        inner.order.push(rec.run_id.clone());
        inner.runs.insert(
            rec.run_id.clone(),
            StoredRun {
                record: rec,
                events: Vec::new(),
                result: None,
            },
        );
        Ok(())
    }

    async fn append_event(&self, run_id: &str, ev: Event) -> Result<(), StoreError> {
        let mut inner = self.inner.lock();
        let run = inner
            .runs
            .get_mut(run_id)
            .ok_or_else(|| StoreError::RunNotFound(run_id.to_string()))?;
        run.events.push(ev);
        Ok(())
    }

    async fn finish_run(&self, run_id: &str, res: RunResult) -> Result<(), StoreError> {
        let mut inner = self.inner.lock();
        let run = inner
            .runs
            .get_mut(run_id)
            .ok_or_else(|| StoreError::RunNotFound(run_id.to_string()))?;
        run.result = Some(res);
        Ok(())
    }
}
