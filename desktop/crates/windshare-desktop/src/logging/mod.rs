mod segments;

use std::io::{self, Write};
use tracing_appender::non_blocking::{ErrorCounter, WorkerGuard};

const LOG_QUEUE_CAPACITY: usize = 1024;

pub struct Logging {
    worker: Option<WorkerGuard>,
    dropped: ErrorCounter,
}

impl Drop for Logging {
    fn drop(&mut self) {
        let dropped = self.dropped.dropped_lines();
        if dropped != 0 {
            tracing::warn!(event = "desktop.log_dropped", dropped);
            eprintln!("desktop.log_dropped count={dropped}");
        }
        drop(self.worker.take());
    }
}

enum Writer {
    File(segments::Segments),
    Stderr,
}

impl Write for Writer {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        if let Self::File(file) = self {
            match file.write(bytes) {
                Ok(written) => return Ok(written),
                Err(error) => {
                    eprintln!("desktop.log_unavailable error={error}");
                    *self = Self::Stderr;
                }
            }
        }
        // Diagnostics remain best effort, including a detached Windows process
        // whose stderr is unavailable. The bounded queue isolates slow sinks.
        let _ = io::stderr().write_all(bytes);
        Ok(bytes.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        if let Self::File(file) = self {
            let _ = file.flush();
        }
        Ok(())
    }
}

pub fn start() -> Logging {
    let file = dirs::cache_dir()
        .ok_or_else(|| io::Error::other("user cache directory unavailable"))
        .and_then(|directory| segments::Segments::open(directory.join("WindShare").join("logs")));
    let writer = match file {
        Ok(file) => Writer::File(file),
        Err(error) => {
            eprintln!("desktop.log_unavailable error={error}");
            Writer::Stderr
        }
    };
    let (writer, guard) = tracing_appender::non_blocking::NonBlockingBuilder::default()
        .buffered_lines_limit(LOG_QUEUE_CAPACITY)
        .lossy(true)
        .finish(writer);
    let dropped = writer.error_counter();
    let filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| "windshare=info".into());
    tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_writer(writer)
        .with_ansi(false)
        .with_target(false)
        .init();
    Logging {
        worker: Some(guard),
        dropped,
    }
}
