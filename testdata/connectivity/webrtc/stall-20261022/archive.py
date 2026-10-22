import hashlib
import json
import pathlib
import statistics

from build_support import ROOT, HERE, BUILD

runs = json.loads((BUILD / 'analysis.json').read_text())['runs']
selected = [r for r in runs if r['status'] == 'passed']
expected = selected[0]['sha256']
assert all(r['sha256'] == expected for r in selected)
normal = {}
fault = {}
for arm in ['before', 'after']:
    rows = [r for r in selected if r['label'].startswith('normal-'+arm)]
    normal[arm] = {
        'runs': len(rows),
        'throughputMBps': [r['MBps'] for r in rows],
        'medianMBps': statistics.median(r['MBps'] for r in rows),
        'firstWriteMs': [r['firstWriteMs'] for r in rows],
        'medianFirstWriteMs': statistics.median(r['firstWriteMs'] for r in rows),
        'sctpCounters': 'not instrumented',
    }
    rows = [r for r in selected if 'double-loss-'+arm in r['label']]
    fault[arm] = {
        'runs': len(rows),
        'retryRecoveryMs': [r['injectedRetryRecoveryMs'] for r in rows],
        't3Timeouts': [r['timeoutsCounter'] for r in rows],
        'kernelDrops': [r['kernelDrops'] for r in rows],
    }
natural = next(r for r in selected if r['label'] == 'pause100-default-2')
timeout = natural['timeouts'][0]
assert len(timeout['outstanding']) == 14
assert all(len(r['transmissions']) == 2 and not any(t['received'] for t in r['transmissions']) for r in timeout['outstanding'])
pause = natural['receiverTrace']['pauseEvents'][0][0] / 1000
offset = natural['packetClockOffsetBoundsUs']
episode = {
    'label': natural['label'],
    't3SenderUs': timeout['atUs'],
    'lastAck': timeout['lastAck'],
    'state': dict(zip(['cumulativeTsn','cwndBeforeTimeout','rwnd','inflightBytes','pendingBytes','nextTsn','rtoUs','t3Count'], timeout['state'])),
    'clockOffsetReceiverMinusSenderUs': offset,
    'injectedPauseAfterT3MsBounds': [(pause-offset[1]-timeout['atUs'])/1000, (pause-offset[0]-timeout['atUs'])/1000],
    'kernelDrops': natural['kernelDrops'],
    'unreceivedDataRecords': natural['unreceivedDataRecords'],
    'missingBursts': natural['missingBursts'],
    'outstanding': timeout['outstanding'],
}
records = []
for run in runs:
    row = {key:run.get(key) for key in [
        'label','status','report','MBps','firstWriteMs','sha256',
        'timeoutsCounter','fastRetransmissions','kernelDrops','unreceivedDataRecords',
        'unreceivedReverseRecords','repeatedDataChunks','maxGatherPackets',
        'injectedRetryRecoveryMs',
    ]}
    directory = (ROOT / run['report']).parent
    row['rawSHA256'] = {
        name:hashlib.sha256((directory/name).read_bytes()).hexdigest()
        for name in ['report.json','native-stats.ndjson','sender.dtls.ndjson','sender.sctp.ndjson','receiver.bin']
        if (directory/name).exists()
    }
    row['senderTraceOmitted'] = [r.get('omitted') for r in run.get('senderTraceMetadata', [])]
    receiver = run.get('receiverTrace')
    row['receiverTraceOmitted'] = receiver['header'][1] if receiver else None
    # Free-form errors and cleanup paths contain machine identities. Keep them in
    # the local reports identified by rawSHA256, outside the published ledger.
    row['errorRecorded'] = bool(run.get('error'))
    cleanup = run.get('cleanup')
    row['remoteCleanup'] = {key:cleanup.get(key) for key in ['event','status']} if cleanup else None
    records.append(row)
evidence = {
    'date':'2026-10-22',
    'scope':'Windows native sender to Linux Chrome 155 over one verified LAN UDP WebRTC peer; 256 MiB, 1 MiB blocks, OPFS. Setup and final hash excluded from throughput.',
    'passedRuns':len(selected),
    'failedReportsRetained':len(runs)-len(selected),
    'fixtureSHA256':expected,
    'normalABBA':normal,
    'doubleLoss':fault,
    'naturalTimeout':episode,
    'repairMatchesProductionAssociation': hashlib.sha256((ROOT/'third_party/pion/sctp/association.go').read_bytes()).hexdigest(),
    'records':records,
}
(HERE / 'results.json').write_text(json.dumps(evidence, indent=2)+'\n')
