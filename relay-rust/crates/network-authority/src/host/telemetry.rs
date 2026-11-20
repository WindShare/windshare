use serde_json::{Map, Value};
use std::fmt;
use tracing::{
    Event, Level, Metadata, Subscriber,
    field::{Field, Visit},
    level_filters::LevelFilter,
    span,
};

#[cfg(target_arch = "wasm32")]
const LOG_LEVEL_ENV: &str = "AUTHORITY_LOG_LEVEL";

/// Parses `AUTHORITY_LOG_LEVEL`, defaulting to `INFO` so steady-state Workers
/// record lifecycle decisions without debug-volume noise.
pub fn parse_level(raw: Option<&str>) -> LevelFilter {
    let Some(value) = raw.map(str::trim) else {
        return LevelFilter::INFO;
    };
    if value.eq_ignore_ascii_case("off") {
        LevelFilter::OFF
    } else if value.eq_ignore_ascii_case("error") {
        LevelFilter::ERROR
    } else if value.eq_ignore_ascii_case("warn") {
        LevelFilter::WARN
    } else if value.eq_ignore_ascii_case("debug") {
        LevelFilter::DEBUG
    } else if value.eq_ignore_ascii_case("trace") {
        LevelFilter::TRACE
    } else {
        LevelFilter::INFO
    }
}

pub struct JsonSubscriber<W> {
    max_level: LevelFilter,
    write: W,
}

impl<W> JsonSubscriber<W>
where
    W: Fn(Level, String) + Send + Sync + 'static,
{
    pub const fn new(max_level: LevelFilter, write: W) -> Self {
        Self { max_level, write }
    }
}

#[derive(Default)]
struct JsonVisitor {
    fields: Map<String, Value>,
}

impl Visit for JsonVisitor {
    fn record_f64(&mut self, field: &Field, value: f64) {
        let number = serde_json::Number::from_f64(value).map_or(Value::Null, Value::Number);
        self.fields.insert(field.name().to_owned(), number);
    }

    fn record_i64(&mut self, field: &Field, value: i64) {
        self.fields
            .insert(field.name().to_owned(), Value::from(value));
    }

    fn record_u64(&mut self, field: &Field, value: u64) {
        self.fields
            .insert(field.name().to_owned(), Value::from(value));
    }

    fn record_i128(&mut self, field: &Field, value: i128) {
        self.fields
            .insert(field.name().to_owned(), Value::from(value.to_string()));
    }

    fn record_u128(&mut self, field: &Field, value: u128) {
        self.fields
            .insert(field.name().to_owned(), Value::from(value.to_string()));
    }

    fn record_bool(&mut self, field: &Field, value: bool) {
        self.fields
            .insert(field.name().to_owned(), Value::from(value));
    }

    fn record_str(&mut self, field: &Field, value: &str) {
        self.fields
            .insert(field.name().to_owned(), Value::from(value));
    }

    fn record_error(&mut self, field: &Field, value: &(dyn std::error::Error + 'static)) {
        self.fields
            .insert(field.name().to_owned(), Value::from(value.to_string()));
    }

    fn record_debug(&mut self, field: &Field, value: &dyn fmt::Debug) {
        self.fields
            .insert(field.name().to_owned(), Value::from(format!("{value:?}")));
    }
}

pub fn format_event(event: &Event<'_>) -> String {
    let mut visitor = JsonVisitor::default();
    event.record(&mut visitor);
    let metadata = event.metadata();
    serde_json::json!({
        "level": metadata.level().as_str(),
        "target": metadata.target(),
        "fields": visitor.fields,
    })
    .to_string()
}

impl<W> Subscriber for JsonSubscriber<W>
where
    W: Fn(Level, String) + Send + Sync + 'static,
{
    fn enabled(&self, metadata: &Metadata<'_>) -> bool {
        *metadata.level() <= self.max_level
    }

    fn max_level_hint(&self) -> Option<LevelFilter> {
        Some(self.max_level)
    }

    fn new_span(&self, _span: &span::Attributes<'_>) -> span::Id {
        // Authority emits structured events only; spans are not retained.
        span::Id::from_u64(1)
    }

    fn record(&self, _span: &span::Id, _values: &span::Record<'_>) {}

    fn record_follows_from(&self, _span: &span::Id, _follows: &span::Id) {}

    fn event(&self, event: &Event<'_>) {
        if !self.enabled(event.metadata()) {
            return;
        }
        (self.write)(*event.metadata().level(), format_event(event));
    }

    fn enter(&self, _span: &span::Id) {}

    fn exit(&self, _span: &span::Id) {}
}

#[cfg(target_arch = "wasm32")]
pub(super) fn install(env: &worker::Env) {
    use std::sync::Once;

    // Install once per isolate so steady-state requests avoid crossing JS FFI
    // to re-read environment bindings on every fetch.
    static INSTALLED: Once = Once::new();
    INSTALLED.call_once(|| {
        let configured = env.var(LOG_LEVEL_ENV).ok().map(|value| value.to_string());
        let level = parse_level(configured.as_deref());
        let subscriber = JsonSubscriber::new(level, |level, line| match level {
            Level::ERROR => worker::console_error!("{line}"),
            Level::WARN => worker::console_warn!("{line}"),
            Level::INFO => worker::console_log!("{line}"),
            Level::DEBUG | Level::TRACE => worker::console_debug!("{line}"),
        });
        let _ = tracing::subscriber::set_global_default(subscriber);
    });
}
