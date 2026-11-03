use gpui::profiler::{self, FrameEvent, FrameTimingCollector};
use std::{
    fmt::Write as _,
    fs::File,
    io::{BufWriter, Write as _},
    path::PathBuf,
    sync::{OnceLock, mpsc},
    thread::{self, JoinHandle},
    time::{Duration, Instant},
};

const DRAIN_INTERVAL: Duration = Duration::from_millis(100);
const MARKER_CAPACITY: usize = 64;
static MARKERS: OnceLock<mpsc::SyncSender<Message>> = OnceLock::new();

enum Message {
    Phase {
        at: Instant,
        operation: usize,
        phase: String,
    },
    Stop,
}

pub struct Capture {
    worker: Option<JoinHandle<()>>,
}

pub fn start() -> Capture {
    let output = std::env::var_os("WINDSHARE_MOTION_PROFILE").map_or_else(
        || std::env::temp_dir().join(format!("windshare-motion-{}.csv", std::process::id())),
        PathBuf::from,
    );
    let file = File::create(&output).expect("motion profile output must be writable");
    let (sender, receiver) = mpsc::sync_channel(MARKER_CAPACITY);
    MARKERS
        .set(sender)
        .unwrap_or_else(|_| panic!("motion capture already started"));
    let origin = Instant::now();
    let collector = FrameTimingCollector::new();
    profiler::set_trace_enabled(true);
    tracing::info!(event = "desktop.motion_profile_started", session = std::process::id(), output = %output.display());
    let worker = thread::spawn(move || {
        if let Err(error) = record(file, &receiver, collector, origin) {
            tracing::error!(event = "desktop.motion_profile_failed", session = std::process::id(), %error);
        }
    });
    Capture {
        worker: Some(worker),
    }
}

pub fn mark(operation: usize, phase: impl Into<String>) {
    if let Some(sender) = MARKERS.get() {
        let _ = sender.try_send(Message::Phase {
            at: Instant::now(),
            operation,
            phase: phase.into(),
        });
    }
}

impl Drop for Capture {
    fn drop(&mut self) {
        if let Some(sender) = MARKERS.get() {
            let _ = sender.send(Message::Stop);
        }
        if let Some(worker) = self.worker.take() {
            let _ = worker.join();
        }
        profiler::set_trace_enabled(false);
    }
}

fn record(
    file: File,
    receiver: &mpsc::Receiver<Message>,
    mut collector: FrameTimingCollector,
    origin: Instant,
) -> std::io::Result<()> {
    let mut output = BufWriter::new(file);
    writeln!(
        output,
        "event,at_ms,draw_ms,layout_ms,prepaint_ms,paint_ms,interval_ms,present_ms,refresh_ms,operation,phase"
    )?;
    loop {
        let message = receiver.recv_timeout(DRAIN_INTERVAL);
        let mut batch = String::new();
        for event in collector.collect_unseen() {
            match event {
                FrameEvent::Draw(frame) => {
                    writeln!(
                        batch,
                        "draw,{},{},{},{},{},0,0,{},0,",
                        milliseconds(frame.draw_start.saturating_duration_since(origin)),
                        milliseconds(frame.draw_duration()),
                        milliseconds(frame.phases.request_layout),
                        milliseconds(frame.phases.prepaint),
                        milliseconds(frame.phases.paint),
                        milliseconds(frame.refresh_interval.unwrap_or_default()),
                    )
                    .expect("writing a string cannot fail");
                }
                FrameEvent::Present(frame) => {
                    writeln!(
                        batch,
                        "present,{},0,0,0,0,{},{},0,0,",
                        milliseconds(frame.present_start.saturating_duration_since(origin)),
                        milliseconds(frame.animation_interval.unwrap_or_default()),
                        milliseconds(frame.present_duration()),
                    )
                    .expect("writing a string cannot fail");
                }
            }
        }
        if let Ok(Message::Phase {
            at,
            operation,
            phase,
        }) = &message
        {
            writeln!(
                batch,
                "phase,{},0,0,0,0,0,0,0,{operation},{phase}",
                milliseconds(at.saturating_duration_since(origin))
            )
            .expect("writing a string cannot fail");
        }
        output.write_all(batch.as_bytes())?;
        // Flush off the UI thread so disk latency does not enter the frame budget.
        output.flush()?;
        if matches!(
            message,
            Ok(Message::Stop) | Err(mpsc::RecvTimeoutError::Disconnected)
        ) {
            return Ok(());
        }
    }
}

fn milliseconds(duration: Duration) -> f64 {
    duration.as_secs_f64() * 1000.
}
