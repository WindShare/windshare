import collections, json, pathlib, statistics

ROOT = pathlib.Path(__file__).resolve().parents[4]
RUNS = ROOT / '.tmp/webrtc-readiness-20261022/runs'
results = []
for path in sorted(RUNS.glob('*/report.json')):
    report = json.loads(path.read_text())
    if report.get('status') != 'passed' or report['retest']['stage'] != 'profile':
        continue
    row = report['rounds'][0]
    start = row['timeOrigin'] + row['started']
    end = start + row['elapsedMs']
    port = row['peers'][0]['selected']['local']['port']
    clock = report['udpClock']
    offset = [clock['remote'] - clock['received'], clock['remote'] - clock['sent']]
    midpoint = statistics.mean(offset)
    samples, drops, previous = [], [], {}
    for sample in report['udpTimeline']:
        if sample['event'] != 'sample':
            continue
        for socket in sample['sockets']:
            if socket['port'] != port:
                continue
            point = {'atMs': sample['time'] - start, **socket}
            samples.append(point)
            identity = socket['family'], socket['inode']
            prior = previous.get(identity)
            if prior is not None and socket['drops'] > prior['drops']:
                drops.append({'fromMs': prior['atMs'], 'toMs': point['atMs'],
                              'count': socket['drops'] - prior['drops'], 'rxBytesBefore': prior['rxBytes'],
                              'rxBytesAfter': socket['rxBytes']})
            previous[identity] = point
    native = [json.loads(line) for line in (path.parent / 'native-stats.ndjson').read_text().splitlines()]
    native = [sample for sample in native if (sample.get('pair') or {}).get('Remote', {}).get('port') == port]
    bulk = [sample for sample in native if 1024 * 1024 < sample['sctp']['bytesSent'] < row['size']]
    timeouts = []
    for index in range(1, len(native)):
        current, prior = native[index], native[index - 1]
        increase = current['loss']['t3Timeouts'] - prior['loss']['t3Timeouts']
        if increase <= 0:
            continue
        timeouts.append({'count': increase,
            'earliestMs': prior['time'] + offset[0] - start, 'latestMs': current['time'] + offset[1] - start,
            'buffered': current['buffered'], 'cwndBefore': prior['sctp']['congestionWindow'],
            'cwndAfter': current['sctp']['congestionWindow'],
            'sentBytesBefore': prior['sctp']['bytesSent'], 'sentBytesAfter': current['sctp']['bytesSent']})
    stalls = []
    for prior, current in zip(native, native[1:]):
        if current['sctp']['bytesSent'] == prior['sctp']['bytesSent'] and current['buffered'] > 0:
            stalls.append({'fromMs': prior['time'] + midpoint - start, 'toMs': current['time'] + midpoint - start,
                           'buffered': current['buffered'], 'cwnd': current['sctp']['congestionWindow']})
    profile = json.loads((path.parent / '0-cpu-profile.json').read_text())['profile']
    nodes = {node['id']: node for node in profile['nodes']}
    parents = {child: node['id'] for node in profile['nodes'] for child in node.get('children', [])}
    inclusive = collections.Counter()
    for sample, delta in zip(profile['samples'], profile['timeDeltas']):
        while sample:
            inclusive[sample] += delta
            sample = parents.get(sample)
    drain = sum(value for key, value in inclusive.items() if nodes[key]['callFrame']['functionName'] == '#drain'
                and '/content/scheduling/admission.ts' in nodes[key]['callFrame']['url']) / 1000
    results.append({'arm': report['retest']['arm'], 'report': path.relative_to(ROOT).as_posix(),
        'MBps': row['MBps'], 'firstOutputMs': row['firstWriteMs'], 'drainSampleMs': drain,
        'clockOffsetBoundsMs': offset, 'selectedPort': port, 'socketSamples': len(samples),
        'socketFirstSampleMs': samples[0]['atMs'] if samples else None,
        'peakSocketReceiveMemoryBytes': max((sample['rxBytes'] for sample in samples if 0 <= sample['atMs'] <= row['elapsedMs']), default=0),
        'socketDropCount': sum(event['count'] for event in drops), 'dropWindows': drops,
        'medianCongestionWindowBytes': statistics.median(sample['sctp']['congestionWindow'] for sample in bulk),
        'medianRoundTripMilliseconds': statistics.median(sample['sctp']['smoothedRoundTripTime'] * 1000 for sample in bulk),
        'fastRetransmissions': native[-1]['loss']['fastRetransmissions'] - native[0]['loss']['fastRetransmissions'],
        'tcpControlMBps': [control['MBps'] for control in report['tcpControls']],
        'timeouts': timeouts, 'queuedZeroProgressSamples': stalls})
output = {'runs': results, 'scope': 'Diagnostic only. Socket receive queue bytes are kernel accounting. UDP sampled every 20 ms and native SCTP every 200 ms. Clock bounds include SSH startup round-trip uncertainty; timeout windows conservatively include this bound. Same socket port and inode tracked. Correlation cannot identify lost SCTP TSNs or prove the cause of every timeout.'}
(pathlib.Path(__file__).parent / 'transport.json').write_text(json.dumps(output, indent=2) + '\n', encoding='utf-8')
for result in results:
    print(json.dumps({key: result[key] for key in ('arm', 'MBps', 'firstOutputMs', 'drainSampleMs', 'clockOffsetBoundsMs', 'socketDropCount', 'peakSocketReceiveMemoryBytes', 'timeouts', 'dropWindows')}))
