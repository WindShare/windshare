use super::{
    Connection, DescriptorDigest, MAX_DESCRIPTOR_BYTES, OperationId, OptionalExtension, Record,
    RelayIdentity, Result, ShareId, Storage, anyhow, ensure, operation_key, params,
};

pub(super) struct Stored {
    pub record: Record,
    pub descriptor: Option<Vec<u8>>,
    pub publication: Option<Vec<u8>>,
}

pub(super) fn read(connection: &Connection, share: ShareId) -> Result<Option<Stored>> {
    let row = connection
        .query_row(
            "SELECT record, descriptor, publication FROM shares WHERE share=?1",
            [share.as_bytes()],
            |row| Ok((row.get::<_, Vec<u8>>(0)?, row.get(1)?, row.get(2)?)),
        )
        .optional()?;
    row.map(|(bytes, descriptor, publication)| {
        Ok(Stored {
            record: Record::decode(&bytes).context("invalid durable lifecycle record")?,
            descriptor,
            publication,
        })
    })
    .transpose()
}
use anyhow::Context;

pub(super) fn validate_record(relay: RelayIdentity, share: ShareId, stored: &Stored) -> Result<()> {
    ensure!(
        stored.record.relay() == relay && stored.record.share() == share,
        "authority key mismatch"
    );
    let registration = match &stored.record {
        Record::Unresolved(record) => Some(record.registration()),
        Record::Published(record) => Some(record.route().registration()),
        Record::Grace(record) => Some(record.route().registration()),
        Record::Stopping(_) | Record::Stopped(_) => None,
    };
    ensure!(
        stored.descriptor.is_some() == stored.publication.is_some(),
        "partial publication"
    );
    if let Some(descriptor) = &stored.descriptor {
        ensure!(
            !descriptor.is_empty() && descriptor.len() <= MAX_DESCRIPTOR_BYTES,
            "descriptor bound"
        );
        ensure!(
            stored
                .publication
                .as_ref()
                .is_some_and(|key| key.len() == 40),
            "invalid publication key"
        );
    }
    if let Some(registration) = registration {
        let descriptor = stored
            .descriptor
            .as_ref()
            .ok_or_else(|| anyhow!("missing descriptor"))?;
        ensure!(
            DescriptorDigest::of(descriptor) == registration.descriptor_digest(),
            "descriptor digest mismatch"
        );
    }
    if matches!(stored.record, Record::Stopped(_)) {
        ensure!(
            stored.descriptor.is_none(),
            "stopped publication retains descriptor"
        );
    }
    Ok(())
}

pub(super) fn apply(
    connection: &Connection,
    relay: RelayIdentity,
    share: ShareId,
    operation: &Storage,
) -> Result<()> {
    let current = read(connection, share)?;
    if let Some(stored) = &current {
        validate_record(relay, share, stored)?;
    }
    match operation {
        Storage::Publish { record, descriptor } => {
            publish(connection, relay, share, current, **record, descriptor)
        }
        Storage::CompareExchange { expected, next } => {
            let actual = current.as_ref().map(|row| row.record);
            let next = next.as_deref().copied();
            ensure!(
                actual == expected.as_deref().copied() || actual == next,
                "authority compare-exchange conflict"
            );
            match next {
                None => {
                    connection.execute("DELETE FROM shares WHERE share=?1", [share.as_bytes()])?;
                }
                Some(record) => {
                    let (descriptor, publication) = if matches!(record, Record::Stopped(_)) {
                        (None, None)
                    } else {
                        current.map_or((None, None), |row| (row.descriptor, row.publication))
                    };
                    let stored = Stored {
                        record,
                        descriptor,
                        publication,
                    };
                    validate_record(relay, share, &stored)?;
                    write(connection, share, &stored)?;
                }
            }
            Ok(())
        }
        Storage::RemovePublication(id) => remove(connection, share, current, *id),
    }
}

fn publish(
    connection: &Connection,
    relay: RelayIdentity,
    share: ShareId,
    current: Option<Stored>,
    record: Record,
    descriptor: &[u8],
) -> Result<()> {
    let Record::Unresolved(unresolved) = record else {
        anyhow::bail!("publication is not unresolved");
    };
    let stored = Stored {
        record,
        descriptor: Some(descriptor.to_vec()),
        publication: Some(operation_key(unresolved.id())),
    };
    validate_record(relay, share, &stored)?;
    if let Some(current) = current {
        ensure!(
            current.record == stored.record
                && current.descriptor == stored.descriptor
                && current.publication == stored.publication,
            "publication conflicts with durable authority"
        );
        return Ok(());
    }
    write(connection, share, &stored)
}

fn remove(
    connection: &Connection,
    share: ShareId,
    current: Option<Stored>,
    id: OperationId,
) -> Result<()> {
    ensure!(id.share() == share, "cleanup share mismatch");
    if let Some(mut stored) = current {
        ensure!(
            id.relay() == stored.record.relay(),
            "cleanup relay mismatch"
        );
        if matches!(stored.record, Record::Unresolved(record) if record.id() == id) {
            connection.execute("DELETE FROM shares WHERE share=?1", [share.as_bytes()])?;
        } else if stored.publication.as_ref() == Some(&operation_key(id)) {
            ensure!(
                matches!(stored.record, Record::Stopping(_)),
                "cleanup would destroy live ownership"
            );
            stored.descriptor = None;
            stored.publication = None;
            write(connection, share, &stored)?;
        }
    }
    Ok(())
}

fn write(connection: &Connection, share: ShareId, stored: &Stored) -> Result<()> {
    connection.execute(
        "INSERT INTO shares (share, record, descriptor, publication) VALUES (?1, ?2, ?3, ?4)
         ON CONFLICT(share) DO UPDATE SET record=excluded.record,
            descriptor=excluded.descriptor, publication=excluded.publication",
        params![
            share.as_bytes(),
            stored.record.encode().as_bytes(),
            stored.descriptor,
            stored.publication
        ],
    )?;
    Ok(())
}
