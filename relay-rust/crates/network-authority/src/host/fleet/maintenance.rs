use super::{Service, origins, signer};
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
    match ctx.path.as_str() {
        "/v1/maintenance/status" => Ok(json!(service.maintenance_status(&parse(bytes)?)?)),
        "/v1/maintenance/drain" | "/v1/maintenance/release" => {
            Ok(json!(service.maintenance_hold(
                &parse(bytes)?,
                ctx.path.ends_with("/drain"),
                &signer(env)?
            )?))
        }
        "/v1/maintenance/probe" => {
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
        _ => Err(Fault::Invalid.into()),
    }
}
