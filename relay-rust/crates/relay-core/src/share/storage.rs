use super::RuntimeStatus;
use super::{
    ConnectionRef, Observer, OperationId, Output, Record, RelayDecision, ShareRelay, Storage, Turn,
    Work, WorkId, WorkResult,
};
use crate::{
    clock::UnixMillis,
    lifecycle::{Event, OwnershipCommit, StopResult, StopWork},
};
use alloc::boxed::Box;
use alloc::vec::Vec;

#[derive(Clone, Debug, Eq, PartialEq)]
pub(super) enum AfterWrite {
    None,
    Ownership {
        id: OperationId,
        replaced: Option<ConnectionRef>,
    },
    // STOP retains a full cleanup identity; keeping it off the queue entry
    // avoids charging that space to every ordinary write, especially on Wasm.
    Stop(Box<StopWork>),
}
#[derive(Clone, Debug, Eq, PartialEq)]
pub(super) enum TaskKind {
    Register {
        ownership: OwnershipCommit,
        descriptor: Vec<u8>,
    },
    Write {
        next: Option<Record>,
        after: AfterWrite,
    },
}
#[derive(Debug)]
pub(super) struct Task {
    kind: TaskKind,
    dispatch: Dispatch,
}
#[derive(Debug)]
enum Dispatch {
    Queued,
    Executing(Work),
    Retry(Work),
}

impl<O: Observer> ShareRelay<O> {
    pub(super) fn enqueue(&mut self, kind: TaskKind, _turn: &mut Turn) {
        if !self.tasks.iter().any(|task| task.kind == kind) {
            self.tasks.push_back(Task {
                kind,
                dispatch: Dispatch::Queued,
            });
        }
    }

    pub(super) fn start_work(&mut self, now: UnixMillis, turn: &mut Turn) {
        let Some(task) = self.tasks.front_mut() else {
            return;
        };
        if !matches!(task.dispatch, Dispatch::Queued) {
            return;
        }
        let Some(sequence) = self.last_work.checked_add(1) else {
            self.runtime = RuntimeStatus::ReloadRequired;
            turn.outputs.push(Output::Reload);
            return;
        };
        self.last_work = sequence;
        let operation = match &task.kind {
            TaskKind::Register {
                ownership,
                descriptor,
            } => Storage::Register {
                ownership: Box::new(ownership.ownership()),
                descriptor: descriptor.clone(),
            },
            TaskKind::Write { next, .. } => Storage::CompareExchange {
                expected: self.stored.map(Box::new),
                next: next.map(Box::new),
            },
        };
        let work = Work {
            id: WorkId {
                incarnation: self.identity.incarnation,
                sequence,
            },
            operation,
        };
        task.dispatch = Dispatch::Executing(work.clone());
        let id = work.id;
        turn.outputs.push(Output::Work(work));
        self.trace(None, RelayDecision::WorkIssued(id), now);
    }

    pub(super) fn retry(&mut self, id: WorkId, now: UnixMillis, turn: &mut Turn) {
        if let Some(task) = self.tasks.front_mut()
            && let Dispatch::Retry(work) = &task.dispatch
            && work.id == id
        {
            let work = work.clone();
            task.dispatch = Dispatch::Executing(work.clone());
            turn.outputs.push(Output::Work(work));
            self.trace(None, RelayDecision::WorkIssued(id), now);
            return;
        }
        self.trace(None, RelayDecision::StaleCompletion(id), now);
    }

    pub(super) fn complete(
        &mut self,
        id: WorkId,
        result: WorkResult,
        now: UnixMillis,
        turn: &mut Turn,
    ) {
        let matches = self.tasks.front().is_some_and(
            |task| matches!(&task.dispatch, Dispatch::Executing(work) if work.id == id),
        );
        if !matches {
            self.trace(None, RelayDecision::StaleCompletion(id), now);
            return;
        }
        if result != WorkResult::Complete {
            let task = self.tasks.front_mut().expect("matched work");
            let Dispatch::Executing(work) =
                core::mem::replace(&mut task.dispatch, Dispatch::Queued)
            else {
                unreachable!("matched work");
            };
            task.dispatch = Dispatch::Retry(work);
            turn.outputs.push(Output::Retry(id));
            self.trace(None, RelayDecision::StorageUncertain(id), now);
            return;
        }
        self.trace(None, RelayDecision::WorkCompleted { id, result }, now);
        let task = self.tasks.pop_front().expect("matched work");
        match task.kind {
            TaskKind::Register { ownership, .. } => {
                self.stored = Some(ownership.record());
                self.after_write(
                    AfterWrite::Ownership {
                        id: ownership.id(),
                        replaced: ownership.replaced(),
                    },
                    now,
                    turn,
                );
            }
            TaskKind::Write { next, after } => {
                self.stored = next;
                self.after_write(after, now, turn);
            }
        }
    }

    fn after_write(&mut self, after: AfterWrite, now: UnixMillis, turn: &mut Turn) {
        match after {
            AfterWrite::None => {}
            AfterWrite::Ownership { id, replaced } => {
                if let Some(connection) = replaced {
                    self.close(connection, turn);
                }
                self.lifecycle_event(Event::OwnershipCommitted(id), None, now, turn);
            }
            AfterWrite::Stop(work) => self.lifecycle_event(
                Event::StopFinished {
                    work: *work,
                    result: StopResult::Complete,
                },
                None,
                now,
                turn,
            ),
        }
    }

    pub(super) fn progress_stop(&mut self, now: UnixMillis, turn: &mut Turn) {
        // STOP fences the whole share, including queues from older owners and
        // detached generations. Settling ledgers keep their charge independently
        // of this barrier; waiting for their removal would make STOP unfinishable
        // on hosts that cannot prove transport-buffer release.
        let ready = self.stop_barrier.is_some()
            && self
                .generations
                .ledgers()
                .all(crate::flow::ConnectionLedger::queues_discarded);
        if ready {
            let work = self.stop_barrier.take().expect("ready stop barrier");
            self.lifecycle_event(
                Event::StopFinished {
                    work,
                    result: StopResult::Complete,
                },
                None,
                now,
                turn,
            );
        }
    }
}
