use super::NodeJsonFormat;
use std::{
    io,
    sync::{Arc, Mutex, OnceLock},
};

#[derive(Clone, Default)]
struct BufferWriter(Arc<Mutex<Vec<u8>>>);

impl io::Write for BufferWriter {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        self.0.lock().unwrap().extend_from_slice(buf);
        Ok(buf.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

#[test]
fn node_json_format_injects_node_id_once_identity_is_known() {
    let node_id_json = Arc::new(OnceLock::new());
    let buffer = BufferWriter::default();
    let writer = buffer.clone();
    let subscriber = tracing_subscriber::fmt()
        .json()
        .event_format(NodeJsonFormat::new(Arc::clone(&node_id_json)))
        .with_writer(move || writer.clone())
        .finish();
    let dispatch = tracing::Dispatch::new(subscriber);

    tracing::dispatcher::with_default(&dispatch, || {
        tracing::info!(event = "before_identity");
        let _ = node_id_json.set(serde_json::to_string("node-abc-123").unwrap());
        tracing::info!(event = "after_identity", detail = 7);
    });

    let raw = String::from_utf8(buffer.0.lock().unwrap().clone()).unwrap();
    let lines: Vec<serde_json::Value> = raw
        .lines()
        .map(|line| serde_json::from_str(line).unwrap())
        .collect();
    assert_eq!(lines.len(), 2);
    assert!(lines[0].get("node_id").is_none());
    assert_eq!(lines[0]["fields"]["event"], "before_identity");
    assert_eq!(lines[1]["node_id"], "node-abc-123");
    assert_eq!(lines[1]["fields"]["event"], "after_identity");
    assert_eq!(lines[1]["fields"]["detail"], 7);
}
