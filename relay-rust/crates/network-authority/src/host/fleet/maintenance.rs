use super::{Service, origins, route::MaintenanceRoute, signer};
use crate::{
    Fault,
    fleet::{maintenance::Operation, vantage::ORIGIN_COUNT},
    host::routes::{Context, parse},
};
use anyhow::{Result, ensure};
use serde::Deserialize;
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use worker::Env;

pub(super) fn dispatch(
    service: &mut Service,
    env: &Env,
    route: MaintenanceRoute,
    ctx: &Context,
    bytes: &[u8],
) -> Result<Value> {
    let secret = env
        .secret("FLEET_MAINTENANCE_TOKEN")
        .ok()
        .map(|v| v.to_string());
    ensure!(
        secret.is_some_and(|s| s.len() >= 32
            && Sha256::digest(s.as_bytes()) == Sha256::digest(ctx.bearer.as_bytes())),
        Fault::Denied {
            reason: "maintenance_credential".into()
        }
    );
    crate::fleet::publication::Publication::load_or_import(&mut service.store)?;
    match route {
        MaintenanceRoute::Status => Ok(json!(service.maintenance_status(&parse(bytes)?)?)),
        MaintenanceRoute::Drain => Ok(json!(service.maintenance_hold(
            &parse(bytes)?,
            true,
            &signer(env)?
        )?)),
        MaintenanceRoute::Release => Ok(json!(service.maintenance_hold(
            &parse(bytes)?,
            false,
            &signer(env)?
        )?)),
        MaintenanceRoute::Probe => {
            #[derive(Deserialize)]
            #[serde(deny_unknown_fields)]
            struct Probe {
                operation: Operation,
                shares: [[u8; 12]; ORIGIN_COUNT],
            }
            let request: Probe = parse(bytes)?;
            Ok(json!(service.maintenance_probe(
                &request.operation,
                &request.shares,
                origins()?
            )?))
        }
    }
}
