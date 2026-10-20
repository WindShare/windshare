use super::{AccessObject, Database, failure, io, trace};
use serde_json::json;
use worker::{Method, Request, RequestInit, Result};

impl AccessObject {
    pub(super) async fn terminate_shares(&self, db: &Database) -> Result<()> {
        for (credential, share) in db.terminations(io::now().0)? {
            db.attempting(&credential, &share, io::now().0)?;
            let result = async {
                let request = Request::new_with_init(
                    "https://internal/internal/terminate",
                    RequestInit::new()
                        .with_method(Method::Post)
                        .with_body(Some(json!({"credential_id":credential}).to_string().into())),
                )?;
                let response = self
                    .env
                    .durable_object("SHARES")?
                    .id_from_name(&share)?
                    .get_stub()?
                    .fetch_with_request(request)
                    .await?;
                if response.status_code() != 200 {
                    return Err(failure(format!(
                        "termination HTTP {}",
                        response.status_code()
                    )));
                }
                db.terminated(&credential, &share)
            }
            .await;
            match result {
                Ok(()) => trace(
                    "credential_share_terminated",
                    &json!({"credential_id":credential,"share_id":share}),
                ),
                Err(error) => trace(
                    "credential_share_termination_retry",
                    &json!({"credential_id":credential,"share_id":share,
                        "retry_after_ms":super::storage::TERMINATION_RETRY_MS,"reason":error.to_string()}),
                ),
            }
        }
        Ok(())
    }
}
