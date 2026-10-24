"""Keep compact, auditable evidence; never combine different frozen revisions."""
from datetime import datetime
import hashlib
import json
from pathlib import Path
import statistics
import sys

ROOT = Path(__file__).resolve().parents[4]
HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
from evidence_export import write_public
FINAL = 'webrtc-adaptive-20261023-settled'

def read(path):
    return json.loads(path.read_text(encoding='utf-8-sig'))

def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

def reference(path):
    return {'path': path.relative_to(ROOT).as_posix(), 'sha256': sha(path)}

def select(value, keys):
    return {key: value[key] for key in keys if key in value}

def peer(value):
    selected = value.get('selected', {})
    return {
        'state': value.get('connectionState'),
        'pair': select(selected.get('pair', {}), ['state', 'bytesReceived', 'bytesSent', 'currentRoundTripTime', 'nominated']),
        **{side: select(selected.get(side, {}), ['address', 'port', 'candidateType', 'protocol', 'relayProtocol', 'networkType']) for side in ['local', 'remote']},
    }

def browser_round(value):
    result = select(value, ['size', 'MBps', 'sha256', 'activationToFirstWriteMs', 'activationToCompleteMs',
        'lifecycle', 'workerStatus', 'fault', 'interactive', 'errors', 'dispatches', 'admissions'])
    result['peers'] = [peer(item) for item in value.get('peers', [])]
    result['parallelism'] = [item for item in value.get('protocolTraces', []) if item.get('stage') == 'parallelism']
    samples = value.get('demandTimeline', [])
    if samples:
        result['demandSamples'] = len(samples)
        result['demandPeak'] = {key: max(item.get(key, 0) for item in samples) for key in
            ['queuedBytes', 'networkWaitingBytes', 'capacityWaitingBytes', 'inFlightBytes', 'unfinishedBytes']}
    return result

def browser_report(path):
    value = read(path)
    return {**reference(path), **select(value, ['status', 'error', 'label', 'study', 'scope', 'browser', 'remoteCleanup']),
        'rounds': [browser_round(item) for item in value.get('rounds', [])]}

def describe(values):
    return {'n': len(values), 'median': statistics.median(values), 'minimum': min(values), 'maximum': max(values)} if values else None

def summarize_matrix(ledger):
    groups = {}
    for run in ledger['runs']:
        if run.get('status') != 'passed': continue
        key = run['scenario'] + '/' + run['arm']
        group = groups.setdefault(key, {'MBps': [], 'firstOutputMs': [], 'completionMs': [], 'peerCounts': []})
        for item in run.get('rounds', []):
            for metric in group:
                value = len(item['peers']) if metric == 'peerCounts' else item.get({
                    'firstOutputMs': 'activationToFirstWriteMs', 'completionMs': 'activationToCompleteMs'}.get(metric, metric))
                if value is not None: group[metric].append(value)
    return {key: {metric: describe(values) for metric, values in group.items()} for key, group in groups.items()}

def matrix(path):
    raw = read(path)
    result = {**reference(path), **select(raw, ['suite', 'status', 'failures', 'startedMs', 'finishedMs', 'machine', 'sourcesSHA256', 'binaries']), 'runs': []}
    for row in raw['runs']:
        run = select(row, ['label', 'scenario', 'arm', 'repetition', 'sizeMiB', 'slowWriteMs', 'interaction', 'fault', 'workflow', 'receivers', 'exitCode', 'status', 'failure', 'startedMs', 'finishedMs'])
        if row.get('report') and (ROOT / row['report']).exists(): run.update(browser_report(ROOT / row['report']))
        result['runs'].append(run)
    result['summary'] = summarize_matrix(result)
    load = path.with_name(path.name.replace('-ledger.json', '-load.ndjson'))
    if load.exists():
        samples = [json.loads(line) for line in load.read_text().splitlines() if line.strip()]
        result['hostLoad'] = {**reference(load), 'scope': 'Windows host-wide 1 Hz samples, including setup/cleanup; not process attribution.',
            **{metric: describe([sample[metric] for sample in samples]) for metric in ['cpuPercent', 'maxCorePercent', 'freeMemory']}}
    return result

def trace_time(value):
    return datetime.fromisoformat(value.replace('Z', '+00:00')).timestamp() * 1000

def native(path):
    raw = read(path)
    result = {**reference(path), **select(raw, ['endpoint', 'direction', 'size', 'sha256', 'scope', 'status', 'error',
        'windowsSHA256', 'linuxSHA256', 'diagnosticOverlay', 'remoteCleanup', 'remoteClock']), 'rounds': []}
    for index, value in enumerate(raw.get('rounds', [])):
        row = select(value, ['repetition', 'receiverStartedMs', 'completionMs', 'exitCode', 'sha256', 'MBpsIncludingStartup'])
        trace = path.parent / f'receiver-{index}.ndjson'
        if trace.exists():
            row['trace'] = reference(trace)
            events = [json.loads(line) for line in trace.read_text(encoding='utf-8-sig').splitlines() if line.strip()]
            row['selectedPairs'] = [select(item, ['elapsed_ms', 'correlation', 'payload']) for item in events if item.get('payload', {}).get('kind') == 'selected_pair']
            row['parallelism'] = [select(item, ['elapsed_ms', 'correlation', 'payload']) for item in events if item.get('payload', {}).get('kind') == 'parallelism']
            first = next((item for item in events if item.get('payload', {}).get('stage') == 'file_first_write'), None)
            if first:
                row['firstOutputFromTraceStartMs'] = float(first['elapsed_ms'])
                if 'receiverStartedMs' in value and (raw['direction'] == 'reverse' or 'remoteClock' in raw):
                    offset = 0 if raw['direction'] == 'reverse' else raw['remoteClock']['offsetMs']
                    row['firstOutputFromLaunchMs'] = trace_time(first['time']) - offset - value['receiverStartedMs']
        result['rounds'].append(row)
    verified = [item for item in result['rounds'] if item.get('exitCode') == 0 and item.get('sha256') == raw['sha256']]
    result['summary'] = {metric: describe([item[metric] for item in verified if metric in item]) for metric in
        ['MBpsIncludingStartup', 'completionMs', 'firstOutputFromLaunchMs']}
    return result

def main():
    document = {
        'date': '2026-10-23', 'finalBuild': FINAL,
        'units': 'MB/s is decimal 1000000 bytes/second; MiB is 1048576 bytes.',
        'timing': 'Browser activation to first sink acceptance/publication; native receiver process launch to process completion, with first checkpointed range from trace. Cached controls start after all four PCs connect and finish after authentication into memory.',
        'builds': [], 'controls': [],
        'limitations': [
            'LAN is one Windows desktop and one Linux Wi-Fi host. Public route uses one reachable Aliyun host; the configured Korea host rejected SSH authentication.',
            'Archived cached four-PC controls and adaptive OPFS workflows differ in connection policy and output work; their difference is not an isolated pipeline-overhead estimate.',
            'complete snapshot browser runs exposed network-window/consumer misclassification. Its confirmation suite overlapped later build/tests and is excluded from performance conclusions.',
            'Earlier snapshots and the diagnostic overlay are retained as investigation evidence, not pooled with final measurements.',
            'Optional growth explores direct paths. TURN retains its existing quota, delegation, renewal and path-replacement owner.',
            'Observed public native runs exposed accepted-fragment loss during optional carrier retirement. The settled build retains authenticated final-carrier identity and reassigns the block only after exact-operation retirement; malformed completions remain protocol failures.',
            'Observed and settled public reverse transfers exceeded the 180-second study deadline. A subsequent uninstrumented settled run passed in 19.06 seconds; preimplementation controls passed in 43.61 and 29.39 seconds. Both successful and timed-out settled runs selected local host candidates. The cause of variation and repeatable public-route gains remain unestablished; failures are retained and excluded from successful-completion summaries.',
            'Disabling aggregate send capacity in a separate frozen diagnostic build did not resolve the public reverse failure. Another instrumented build completed with optional-path retirement before scheduled stack dumps. Neither diagnostic build is included in performance conclusions.',
            'Two observed forward native runs lost their exit status in the remote Python supervisor before hash verification. They are excluded; the supervisor now joins the child without a buffered stdin daemon.',
        ],
    }
    for build in sorted((ROOT / '.tmp').glob('webrtc-adaptive-20261023*')):
        source = build / 'sources.json'
        if not source.exists(): continue
        manifest = read(source)
        entry = {'name': build.name, 'sources': reference(source), 'revision': manifest['revision'],
            'diagnostic': manifest.get('diagnostic'),
            'binaries': read(build / 'binaries.json') if (build / 'binaries.json').exists() else {},
            'fixture': manifest['fixture'], 'matrices': [], 'native': [], 'turn': []}
        if build.name == FINAL:
            entry['workingSourceMismatches'] = [file for file, expected in manifest['files'].items()
                if file != 'docs/plans/webrtc-throughput.md' and sha(ROOT / file) != expected]
            assert not entry['workingSourceMismatches'], entry['workingSourceMismatches']
        for path in sorted(build.glob('*-ledger.json')): entry['matrices'].append(matrix(path))
        for path in sorted(build.glob('native-*/report.json')): entry['native'].append(native(path))
        for path in sorted(build.glob('public-turn-*/report.json')):
            raw = read(path)
            turn = {**reference(path), **select(raw, ['scope', 'provider', 'status', 'error', 'remoteCleanup']), 'runs': []}
            for row in raw.get('runs', []):
                item = select(row, ['label', 'sizeMiB', 'exitCode', 'status', 'error'])
                if row.get('report'): item.update(browser_report(ROOT / row['report']))
                turn['runs'].append(item)
            entry['turn'].append(turn)
        document['builds'].append(entry)
    for path in sorted((ROOT / '.tmp').glob('webrtc-adaptive-controls-*/ledger.json')):
        raw = read(path)
        control = {**reference(path), **raw}
        control['summary'] = {arm: describe([row['MBps'] for row in raw['runs'] if row['arm'] == arm and 'MBps' in row and not row.get('error')]) for arm in ['cached', 'product']}
        for row in control['runs']:
            if row.get('report'):
                report = ROOT / row['report']
                row['reportSHA256'] = sha(report)
                if row['arm'] == 'product': row['evidence'] = browser_report(report)
                else:
                    detail = read(report)
                    row['remoteCleanup'] = detail.get('remoteCleanup')
                    row['selectedPairs'] = detail['trials'][0].get('result', {}).get('connections', [])
        document['controls'].append(control)
    document['publicReachability'] = [{**reference(path), **read(path)} for path in sorted((ROOT / '.tmp/throughput').glob('public-reachability-*.json'))]
    baseline = ROOT / '.tmp/webrtc-preflight-20261023'
    document['preimplementationPublicNative'] = {
        'sources': reference(baseline / 'sources.json'),
        'binaries': read(baseline / 'binaries.json'),
        'runs': [native(path) for path in sorted(baseline.glob('native-aliyun-*/report.json'))],
    }
    lifecycle = ROOT / '.tmp/throughput/cf-turn-lifecycle.json'
    if lifecycle.exists():
        document['temporaryTurnKey'] = {**reference(lifecycle), **select(read(lifecycle), ['name', 'created', 'status', 'deleted'])}
    gates = ROOT / '.tmp/throughput/settled-gates.json'
    if gates.exists():
        document['validation'] = {**reference(gates), 'gates': read(gates)}
    earlier_gates = ROOT / '.tmp/throughput/final-gates.json'
    if earlier_gates.exists():
        document['preRepairValidation'] = {**reference(earlier_gates), 'gates': read(earlier_gates)}
    document['retirementRegression'] = {
        'scope': 'Accepted fragment lost on a retiring carrier, final migrated to another authenticated carrier, fresh operation reassigned after retirement, exact output verified. Same-carrier missing data and invalid migrated completion counts remain protocol failures.',
        'source': reference(ROOT / 'core/session/sessionruntime/block_retirement_test.go'),
        'raceTestLog': reference(ROOT / '.tmp/throughput/retirement-fix.log'),
    }
    document['harness'] = {path.name: sha(path) for path in HERE.iterdir() if path.suffix in ['.py', '.mjs', '.go']}
    write_public(HERE / 'results.json', document)
    final = next(item for item in document['builds'] if item['name'] == FINAL)
    print(json.dumps({'matrix': [item['summary'] for item in final['matrices']],
        'native': [{**select(item, ['endpoint', 'direction', 'size', 'status']), **item['summary']} for item in final['native']],
        'turn': [select(item, ['status', 'error']) for item in final['turn']],
        'controls': [item['summary'] for item in document['controls']]}, indent=2))

if __name__ == '__main__':
    main()
