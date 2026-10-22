import json, math, pathlib, random, re, statistics

ROOT = pathlib.Path(__file__).resolve().parents[4]
RUNS = ROOT / '.tmp/webrtc-readiness-20261022/runs'
OUT = pathlib.Path(__file__).parent / 'results.json'

def metrics(snapshot):
    return {item['name']: item['value'] for item in snapshot['metrics']['metrics']}

def socket_drops(sample, port):
    lines = sample['ss'].splitlines()
    for index, line in enumerate(lines[:-1]):
        if re.search(r':' + str(port) + r'\s', line):
            match = re.search(r'\bd(\d+)\)', lines[index + 1])
            if match:
                return int(match[1])
    return None

def paired_summary(pairs, key, percent):
    values = [math.log(pair['new'][key] / pair['old'][key]) if percent else pair['new'][key] - pair['old'][key] for pair in pairs]
    if not values:
        return None
    convert = (lambda x: 100 * math.expm1(x)) if percent else (lambda x: x)
    rng = random.Random(20261022)
    bootstrap = sorted(convert(statistics.mean(rng.choices(values, k=len(values)))) for _ in range(10000))
    return {'count': len(values), 'newGreater': sum(value > 0 for value in values),
            'medianPairedChange': statistics.median([convert(value) for value in values]),
            'meanPairedChange': convert(statistics.mean(values)),
            'bootstrap95': [bootstrap[249], bootstrap[9749]], 'unit': 'percent' if percent else 'milliseconds'}

runs, failures = [], []
for path in sorted(RUNS.glob('*/report.json')):
    report = json.loads(path.read_text(encoding='utf-8'))
    if report.get('status') != 'passed':
        failures.append({'report': path.relative_to(ROOT).as_posix(), 'status': report.get('status'), 'error': report.get('error')})
        continue
    for row in report['rounds']:
        before, after = map(metrics, (row['transferMetrics']['before'], row['transferMetrics']['after']))
        port = row['peers'][0]['selected']['local']['port']
        drops = [socket_drops(sample, port) for sample in report['socketSamples']]
        interactive = row.get('interactive')
        runs.append({**report['retest'], 'report': path.relative_to(ROOT).as_posix(),
            'MBps': row['MBps'], 'elapsedMs': row['elapsedMs'], 'firstOutputMs': row['firstWriteMs'],
            'scriptSeconds': after['ScriptDuration'] - before['ScriptDuration'],
            'taskSeconds': after['TaskDuration'] - before['TaskDuration'],
            'admission': row['admissionCounters'], 'sha256': row['sha256'], 'bytes': row['size'],
            'dispatches': len(row['dispatchTimeline']), 'socketDrops': drops[-1] - drops[0] if None not in drops else None,
            'interaction': report['interaction'], 'interactive': interactive, 'peers': len(row['peers']), 'selected': row['peers'][0]['selected'],
            'dnsIsolated': report['remote']['externalDNSBlocked'], 'cleanup': report['remoteCleanup']['status']})

groups = {}
for stage in sorted({run['stage'] for run in runs}):
    groups[stage] = {}
    for arm in ('old', 'new'):
        values = [run for run in runs if run['stage'] == stage and run['arm'] == arm]
        if not values:
            continue
        groups[stage][arm] = {'count': len(values), **{key: statistics.median(run[key] for run in values)
            for key in ('MBps', 'firstOutputMs', 'scriptSeconds', 'taskSeconds')},
            'drains': statistics.median(run['admission']['drains'] for run in values)}
baseline = [run for run in runs if run['stage'] == 'baseline']
pairs = []
for cohort, pair in sorted({(run['cohort'], run['pair']) for run in baseline}):
    arms = {run['arm']: run for run in baseline if run['cohort'] == cohort and run['pair'] == pair}
    if arms.keys() == {'old', 'new'}:
        pairs.append(arms)
paired = {'throughput': paired_summary(pairs, 'MBps', True), 'firstOutput': paired_summary(pairs, 'firstOutputMs', False)}
interactions = {}
for kind in ('preview', 'cancel-preview'):
    interactions[kind] = {}
    for arm in ('old', 'new'):
        values = [run['interactive'] for run in runs if run['interaction'] == kind and run['arm'] == arm]
        if not values:
            continue
        start = 'abortRequestedMs' if kind == 'cancel-preview' else 'startedMs'
        durations = [value['completedMs'] - value[start] for value in values]
        interactions[kind][arm] = {'count': len(values), 'milliseconds': durations, 'medianMs': statistics.median(durations),
                                   'results': [value['result'] for value in values]}
result = {'groups': groups, 'paired': paired, 'interactions': interactions, 'runs': runs, 'failures': failures,
          'scope': 'Frozen before/after browser snapshots; identical production Windows sender, fixture, UDP LAN path and OPFS output. Both arms instrument admission counters. Setup and hash excluded. Profile runs are separate and add CPU and 20 ms UDP socket sampling. Bootstrap describes eight adjacent pairs, not equivalence or general WAN behavior.'}
OUT.write_text(json.dumps(result, indent=2) + '\n', encoding='utf-8')
print(json.dumps({'groups': groups, 'paired': paired, 'interactions': interactions, 'failures': failures}, indent=2))
