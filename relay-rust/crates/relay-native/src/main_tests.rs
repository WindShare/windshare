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

#[test]
fn force_release_cli_dispatches_identity_busy_and_release() {
    use super::{BUSY_EXIT_CODE, run_maintenance_command};
    use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
    use windshare_relay_native::node::Identity;

    let temp = tempfile::tempdir().unwrap();
    let config_path = temp.path().join("node.json");
    let database = temp.path().join("relay.db");
    let identity_file = temp.path().join("identity.json");
    std::fs::write(
        &config_path,
        serde_json::to_vec(&serde_json::json!({
            "listen": "127.0.0.1:8484",
            "database": database,
            "identity_file": identity_file,
            "node": { "services": { "relay": true, "stun": false } }
        }))
        .unwrap(),
    )
    .unwrap();
    let identity = Identity::open(&identity_file).unwrap();
    let node_key = URL_SAFE_NO_PAD.encode(identity.key().verifying_key().to_bytes());
    let wrong_key = URL_SAFE_NO_PAD.encode([5u8; 32]);
    std::fs::write(&database, b"").unwrap();
    let gate_path = database
        .canonicalize()
        .unwrap()
        .with_added_extension("admission");
    std::fs::write(&gate_path, b"operation-stuck-1").unwrap();

    let wrong_arity = vec![
        "--force-release-maintenance".to_string(),
        config_path.display().to_string(),
    ];
    assert_eq!(run_maintenance_command(&wrong_arity).unwrap(), None);

    let wrong_host = vec![
        "--force-release-maintenance".to_string(),
        config_path.display().to_string(),
        wrong_key,
    ];
    assert!(run_maintenance_command(&wrong_host).is_err());

    let args = vec![
        "--force-release-maintenance".to_string(),
        config_path.display().to_string(),
        node_key,
    ];
    let held = std::fs::File::open(&gate_path).unwrap();
    held.try_lock_shared().unwrap();
    assert_eq!(
        run_maintenance_command(&args).unwrap(),
        Some(BUSY_EXIT_CODE)
    );
    drop(held);

    assert_eq!(run_maintenance_command(&args).unwrap(), Some(0));
    assert_eq!(std::fs::metadata(&gate_path).unwrap().len(), 0);
}
