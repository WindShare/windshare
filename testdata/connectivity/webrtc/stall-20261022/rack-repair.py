def apply(source):
    excluded = 'if chunk.retransmit || chunk.nSent > 1 {'
    if source.count(excluded) != 2:
        raise RuntimeError('RACK loss scanner changed')
    source=source.replace(excluded,'if chunk.retransmit {')
    source=source.replace('// Either already scheduled for retransmit or not an original send:\n\t\t\t\t// skip but keep in list in case it\'s still outstanding.',
        '// Already queued work must not be scheduled twice.')
    source=source.replace('// Ordered by original send time.', '// Ordered by most recent transmission time.')
    update='if chunkPayload.since.After(newestDeliveredSendTime) {'
    if source.count(update)!=2:
        raise RuntimeError('RACK delivery evidence changed')
    source=source.replace(update,'if a.rackAcknowledgedTransmissionValid(chunkPayload, now) && chunkPayload.since.After(newestDeliveredSendTime) {')
    source += '''
// A late ACK may belong to the original transmission rather than the retry.
// Keep Karn's RTT sampling rule separate from RACK delivery evidence, and use
// the measured minimum RTT to reject physically implausible retry ACKs.
func (a *Association) rackAcknowledgedTransmissionValid(chunk *chunkPayloadData, now time.Time) bool {
 if chunk.nSent == 1 { return true }
 return a.rackMinRTT > 0 && !now.Before(chunk.since.Add(a.rackMinRTT))
}
'''
    return source
