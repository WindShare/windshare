# Cloudflare TURN integration

## Outcome

Both `full` and `relay-only` deployments automatically provision Cloudflare TURN.
Users complete the existing deployment and `wind relay onboard` flow; no additional
TURN key creation, copying or configuration import is required. Recipients continue
opening ordinary share links. Provisioned TURN participates in automatic scheduling
by default, adding a relay option without forcing traffic or promising faster P2P.

## Architecture

- Model TURN issuance as an authenticated capability of a deployment, separate from
  application-relay transport. Discover it during onboarding and refresh it in the
  background on later connections, without delaying share links or content.
- Keep the deployment API token in GitHub Actions and the TURN key secret in the
  Worker. Devices receive temporary credentials through an authenticated issuer;
  recipients receive them through existing encrypted session control.
- Extend the existing issuer abstraction with a remote credential source. Reuse
  TURN leases, renewal, revocation, admission and scheduling. HTTP and provider
  integration remain outside the network-free `core/**` boundary.
- Cloudflare TURN carries the data; the Worker handles authorization and issuance.
  Direct connections and application relays remain available when TURN fails.

## Execution

1. **Define the managed service contract.** Derive stable service identity from the
   deployment and carry lease identity, endpoints and expiry through the issuer
   contract. Bind issue/renew/revoke to the authenticated device and session.
   Model managed service references separately from device-owned secrets and
   accounts. Managed services follow the configured active relay selection, not
   live connection availability; manual TURN accounts remain independent. Separate
   discovered inventory from runtime admission so onboarding does not consume the
   manual account quota or fail when it is full.

2. **Implement the Worker issuer.** Add capability and credential endpoints to
   `relay-rust/crates/relay-cloudflare`, using existing device access authority and
   request-bound authentication. Validate lease ownership, bound lifetime and
   issuance rate, and persist leases. Recheck authority before publishing issued
   credentials; durably retry revocations until provider confirmation or expiry.
   Revoked devices cannot issue or renew; delegated credentials remain usable until
   expiry or confirmed revocation. Keep lease policy testable separately from Worker
   HTTP/storage adapters and outside the relay kernel.

3. **Connect native clients and onboarding.** Implement the remote issuer in
   `engine/turnprovider` and connect it to `engine/turnaccount`. Include managed
   references in onboarding and console exports. Background refresh preserves
   unrelated settings and still-valid capabilities on timeout; explicit disable or
   revocation withdraws authority. Acquire credentials only for preview/download
   demand. Reuse encrypted browser handoff and renewal without requiring a live
   application-relay connection. Issuer failures retain valid paths while direct
   and application-relay transfers continue.

4. **Automate provisioning.** Extend `scripts/deploy/cloudflare` and the workflow
   with `Calls Write` for TURN provisioning. Persist creation intent and deployment
   ownership before creating the key; save its secret before advertising capability.
   Secrets cannot be read back: recover interrupted publication or replace only
   confirmed deployment-owned, unpublished keys whose secrets were lost. Reuse
   working keys on reruns. Report incomplete TURN setup without blocking relay/
   frontend updates or replacing a working service. Keep secrets out of output
   and client exports.

5. **Expose ownership and usage controls.** Explain automatic use and separate
   Realtime billing during deployment setup. Default local budgets remain unlimited;
   configured budgets apply per Engine, not as account-wide spending caps. Persist
   the console enable/disable choice across redeployments. Disable stops issuance
   and queues outstanding leases for revocation; show pending versus confirmed or
   expired status. Keep analytics optional and outside transfers, using a separate
   read-only credential when available, never the deployment token. Update only the
   relevant deployment/token and TURN guides.

6. **Verify the integrated flow.** Extend existing tests for interrupted provisioning,
   mixed manual/managed configuration, nonblocking discovery failures, unauthorized
   issuance, disable/revocation races and renewal failure. Reuse the local TURN
   scenario from deployment discovery through browser transfer, then run one
   isolated Cloudflare smoke trial and clean up its resources. Emit structured
   provisioning, discovery, issuance and revocation events with deployment, device,
   session and lease identifiers.
