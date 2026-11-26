# P4 — Community nodes

Part of the [Global Network Plan](global-network.md#6-phases).

Volunteers join by running a node, and discovery works without the official operator.

- Inclusion needs no approval: a node publishes its self-signed descriptor and announces itself to a
  few bootstrap nodes, official ones by default. Nodes and clients exchange bounded descriptor
  samples and cache signature-verified descriptors. Self-signatures establish identity, not service
  trust; apply local endpoint policy before enabling advertised STUN. Operator and capacity fields
  remain unverified hints.
- Recommendation affects ranking, not admission. Use P1's bounded registration attempts for community
  candidates too; without reachable recommended nodes, community nodes can serve alone or together.
  Prefer distinct operators and providers without requiring them. Explicit self-hosted relays work
  independently, and users can turn community nodes off.
- Native owner onboarding: initialize admin credentials through a one-time link and sign in.
  Address-only setup uses browser authorization to claim and save the owner key; reuse `relay-access`
  and `engine/relaysetup`. Public admission and owner authorization are independent; manual keys still work.
- Community template: contribution on by default with conservative automatic budgets (daily public
  traffic, share of peak bandwidth, relay connections), editable by the owner. Owner traffic comes
  first and may push community traffic to zero. Reaching the daily public traffic cap stops public
  forwarding. The anonymous class is on; nothing depends on the official authority.
- Cloudflare Workers relays contribute only when their owner enables it, with budgets that count
  platform request limits, not just bytes.
- Eligibility stays soft (中转服务.md): community nodes are offered when the user owns no native
  relay, owns at least one contributing native relay, or owns only Cloudflare relays. Configuring
  access to another person's relay does not establish ownership. Turning off contribution on the
  last contributing native relay shows a one-time notice and pauses new community requests;
  owner saturation, maintenance, or a spent budget does not count as refusal. Nothing else mentions contribution.
- Every node mirrors the signed directory and its descriptor cache.
- Domainless community hosts: evaluate direct TLS in `wsrelay` (built-in ACME with Let's Encrypt
  160-hour IP certificates, ARI renewal, and zero-downtime reload) versus a scripted Caddy proxy.
- Traces: descriptor exchange (source, accepted or rejected, reason) and community picks in planner
  decisions.
