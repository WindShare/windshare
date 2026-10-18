use super::{
    Connection, OptionalExtension, RelayIdentity, Result, ShareId, Storage, anyhow, params,
};
use windshare_relay_core::authority::{self, StoredAuthority};

pub(super) fn read(
    connection: &Connection,
    relay: RelayIdentity,
    share: ShareId,
) -> Result<Option<StoredAuthority>> {
    let row = connection
        .query_row(
            "SELECT record, descriptor FROM shares WHERE share=?1",
            [share.as_bytes()],
            |row| Ok((row.get::<_, Vec<u8>>(0)?, row.get(1)?)),
        )
        .optional()?;
    row.map(|(bytes, descriptor)| {
        StoredAuthority::decode(relay, share, &bytes, descriptor)
            .map_err(|error| anyhow!("invalid durable authority: {error:?}"))
    })
    .transpose()
}

pub(super) fn apply(
    connection: &Connection,
    relay: RelayIdentity,
    share: ShareId,
    operation: &Storage,
) -> Result<Option<StoredAuthority>> {
    let current = read(connection, relay, share)?;
    let next = authority::apply(relay, share, current.as_ref(), operation)
        .map_err(|error| anyhow!("authority transition: {error:?}"))?;
    if next == current {
        return Ok(next);
    }
    if let Some(stored) = &next {
        connection.execute(
            "INSERT INTO shares (share, record, descriptor) VALUES (?1, ?2, ?3)
             ON CONFLICT(share) DO UPDATE SET record=excluded.record,
                descriptor=excluded.descriptor",
            params![
                share.as_bytes(),
                stored.record().encode().as_bytes(),
                stored.descriptor()
            ],
        )?;
    } else {
        connection.execute("DELETE FROM shares WHERE share=?1", [share.as_bytes()])?;
    }
    Ok(next)
}
