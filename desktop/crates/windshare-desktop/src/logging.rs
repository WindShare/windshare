use tracing_appender::{
    non_blocking::WorkerGuard,
    rolling::{Builder, Rotation},
};

pub fn start() -> WorkerGuard {
    let directory = dirs::cache_dir()
        .expect("user cache directory")
        .join("WindShare")
        .join("logs");
    let appender = Builder::new()
        .rotation(Rotation::DAILY)
        .filename_prefix("desktop")
        .filename_suffix("log")
        .max_log_files(7)
        .build(directory)
        .expect("open desktop log");
    let (writer, guard) = tracing_appender::non_blocking::NonBlockingBuilder::default()
        .buffered_lines_limit(1024)
        .lossy(true)
        .finish(appender);
    let filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| "windshare=info".into());
    tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_writer(writer)
        .with_ansi(false)
        .with_target(false)
        .init();
    guard
}
