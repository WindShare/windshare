//! Convert the previous fleet schema before any request can decode node records.
use super::{NodeState, topology::Status};
use crate::{
    Node, Store,
    store::{key, save},
};
use anyhow::{Result, ensure};
use serde::Deserialize;
use windshare_relay_access::usage::{Admission, NodePolicy};

const MIGRATED: &str = "schema/fleet-lifecycle-v2";

#[derive(Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
enum LegacyState {
    Listed {
        status: Status,
        maintenance: Option<String>,
    },
    Staging {
        enrollment: Enrollment,
    },
    Disabled,
}

#[derive(Deserialize)]
#[serde(rename_all = "snake_case")]
enum Enrollment {
    Provisional,
    Disabled,
}

/// Runs inside one storage transaction. A live hold must finish with its original
/// controller so the signed directory and the binary selected on the host agree.
pub fn migrate(store: &mut impl Store) -> Result<()> {
    if store.get(MIGRATED)?.is_some() {
        return Ok(());
    }
    for (record, raw) in store.scan("node/", None)? {
        let mut value: serde_json::Value = serde_json::from_str(&raw)?;
        if value["state"].is_string() {
            continue;
        }
        let state = match serde_json::from_value(value["state"].take())? {
            LegacyState::Listed {
                status,
                maintenance,
            } => {
                ensure!(
                    maintenance.is_none(),
                    "finish the in-flight node upgrade before fleet migration"
                );
                NodeState::from(status)
            }
            LegacyState::Disabled => NodeState::Disabled,
            LegacyState::Staging { enrollment } => {
                // Withdrawing a draft changes its previous draining policy to disabled.
                let version = value["version"]
                    .as_u64()
                    .and_then(|v| v.checked_add(1))
                    .ok_or(crate::Fault::Invalid)?;
                value["version"] = version.into();
                if matches!(enrollment, Enrollment::Provisional) {
                    let id: [u8; 32] = serde_json::from_value(value["key"].clone())?;
                    let anonymous = serde_json::from_value(value["anonymous"].clone())?;
                    save(
                        store,
                        &key("retired-node", &id),
                        &NodePolicy {
                            version,
                            anonymous,
                            admission: Admission::Disabled,
                        },
                    )?;
                    store.delete(&key("node-load", &id))?;
                    store.delete(&record)?;
                    continue;
                }
                NodeState::Disabled
            }
        };
        value["state"] = serde_json::to_value(state)?;
        let node: Node = serde_json::from_value(value)?;
        save(store, &record, &node)?;
    }
    for prefix in ["vantage-lease/", "fleet-cycle/", "maintenance-complete/"] {
        for (record, _) in store.scan(prefix, None)? {
            store.delete(&record)?;
        }
    }
    store.delete("directory/mirrors")?;
    store.put(MIGRATED, "true")?;
    tracing::info!(event = "fleet_lifecycle_migrated");
    Ok(())
}
