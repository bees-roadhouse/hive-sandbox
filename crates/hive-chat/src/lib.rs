//! Messages become agent runs.
//!
//! One run per message, resumed through session_id. A conversation that is
//! idle costs nothing, a crash loses one turn rather than a session, and the
//! cold start per turn is the accepted price of both.

mod answer;
mod hub;
mod worker;

pub use answer::{Answer, assistant_text};
pub use hub::{Frame, Hub, Subscription, TurnUpdate, Update, frame_of, frame_of_record};
pub use worker::{
    Config, FAILED_TURN_NOTICE, LEASE_DURATION, RECLAIMED_TURN_NOTICE, Worker, WorkerError,
    streaming_args,
};
