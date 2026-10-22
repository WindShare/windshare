import json, pathlib, statistics, math, random, re, sys

ROOT = pathlib.Path(__file__).resolve().parents[4]
RUNS = ROOT / '.tmp/webrtc-interleaved-20261022/runs'
OUT = ROOT / 'testdata/connectivity/webrtc/interleaved-20261022/results.json'

def median(values):
    return statistics.median(values) if values else None

def counters(snapshot):
    return {v['name']: v['value'] for v in snapshot['metrics']['metrics']}

def socket(sample, port):
    lines = sample['ss'].splitlines()
    for index, line in enumerate(lines[:-1]):
        if re.search(r':' + str(port) + r'\s', line):
            values = re.search(r'skmem:\(([^)]+)', lines[index + 1])
            if values:
                return {k: int(v) for k,v in re.findall(r'([a-z]+)(\d+)', values[1])}
    return {}

def summarize(report, path):
    row = report['rounds'][0]
    before, after = map(counters, (row['transferMetrics']['before'], row['transferMetrics']['after']))
    metrics = {k: after[k] - before[k] for k in ['ScriptDuration','TaskDuration','ThreadTime','DevToolsCommandDuration']}
    previous = {p['id']: p['cpuTime'] for p in row['transferMetrics']['before']['processes']['processInfo']}
    processes = {}
    for p in row['transferMetrics']['after']['processes']['processInfo']:
        if p['id'] in previous:
            processes[p['type']] = processes.get(p['type'], 0) + p['cpuTime'] - previous[p['id']]
    started = row['started']
    receive = [r for r in row['receiveTimeline'] if r['end'] >= started and r['at'] < started + row['elapsedMs']]
    gaps = [r['at'] - receive[i-1]['end'] for i,r in enumerate(receive) if i > 0]
    writes = row['writeTimeline']
    quarters, count, prior_bytes, prior_time = [], 0, 0, started
    for w in writes:
        count += w['bytes']
        if count - prior_bytes >= 64*1024*1024:
            quarters.append((count-prior_bytes)/(w['end']-prior_time)/1000)
            prior_bytes, prior_time = count, w['end']
    port = row['peers'][0]['selected']['local']['port']
    sockets = [socket(s, port) for s in report.get('socketSamples', [])]
    return {
        **report['retest'], 'cohort': report['retest'].get('cohort','initial'), 'stage': report['retest'].get('stage','baseline'), 'report': str(path.relative_to(ROOT)).replace('\\','/'),
        'status': report['status'], 'MBps': row['MBps'], 'elapsedMs': row['elapsedMs'],
        'firstWriteMs': row['firstWriteMs'], 'outputWriteMs': row['outputWriteMs'],
        'outputCloseTailMs': started + row['elapsedMs'] - writes[-1]['end'],
        'receiveBinEndToOutputCloseMs': max(0, started + row['elapsedMs'] - receive[-1]['end']),
        'receiveGapMaxMs': max(gaps, default=0), 'receiveGapOver50Ms': sum(g for g in gaps if g > 50),
        'quarterMBps': quarters, 'transferMetricsSeconds': metrics, 'processCPUSeconds': processes,
        'socket': sockets, 'socketDrops': sockets[-1].get('d',0)-sockets[0].get('d',0) if sockets else None,
        'tcpMBps': [r['MBps'] for r in report.get('tcpControls', [])],
        'outputControlMBps': report.get('outputControl',{}).get('MBps'),
        'sha256': row['sha256'], 'bytes': row['size'], 'dispatches': len(row['dispatchTimeline']),
        'selectedBefore': row['initialPeers'], 'selectedAfter': [p['selected'] for p in row['peers']],
        'admissionCounters': row.get('admissionCounters'),
        'browser': report['browserVersion'], 'dnsIsolated': report['remote']['externalDNSBlocked'],
        'cleanup': report.get('remoteCleanup'),
    }

runs = []
for path in sorted(RUNS.glob('*/report.json')):
    report = json.loads(path.read_text(encoding='utf-8'))
    if report.get('status') == 'passed' and report.get('retest') and report.get('rounds'):
        runs.append(summarize(report, path))
groups = {}
baseline = [r for r in runs if r['stage'] == 'baseline']
for arm in sorted({r['arm'] for r in baseline}):
    values = [r for r in baseline if r['arm'] == arm]
    groups[arm] = {'count':len(values), 'medianMBps':median([r['MBps'] for r in values]),
                  'medianFirstWriteMs':median([r['firstWriteMs'] for r in values]),
                  'medianOutputWriteMs':median([r['outputWriteMs'] for r in values]),
                  'medianScriptSeconds':median([r['transferMetricsSeconds']['ScriptDuration'] for r in values]),
                  'medianTaskSeconds':median([r['transferMetricsSeconds']['TaskDuration'] for r in values])}
pairs = []
for cohort,pair in sorted({(r['cohort'],r['pair']) for r in baseline}, key=lambda value:(value[0],int(value[1]))):
    pair_runs = {r['arm']:r for r in baseline if r['cohort'] == cohort and r['pair'] == pair}
    if set(pair_runs) == {'old','new'}:
        old, new = pair_runs['old'], pair_runs['new']
        pairs.append({'cohort':cohort,'pair':pair,'oldMBps':old['MBps'],'newMBps':new['MBps'],
                      'differencePercent':100*(new['MBps']/old['MBps']-1)})
ratios = [math.log(p['newMBps']/p['oldMBps']) for p in pairs]
paired = {'count':len(pairs), 'newWins':sum(p['differencePercent']>0 for p in pairs)}
if ratios:
    paired['geometricMeanDifferencePercent'] = 100*math.expm1(statistics.mean(ratios))
    rng = random.Random(20261022)
    samples = sorted(100*math.expm1(statistics.mean(rng.choices(ratios,k=len(ratios)))) for _ in range(10000))
    paired['pairedBootstrap95Percent'] = [samples[249],samples[9749]]
stages = {}
for stage in sorted({r['stage'] for r in runs if r['stage'] != 'baseline'}):
    stages[stage] = {}
    for arm in sorted({r['arm'] for r in runs if r['stage'] == stage}):
        values = [r for r in runs if r['stage'] == stage and r['arm'] == arm]
        stages[stage][arm] = {'count':len(values),'medianMBps':median([r['MBps'] for r in values]),
            'medianScriptSeconds':median([r['transferMetricsSeconds']['ScriptDuration'] for r in values]),
            'medianDrains':median([r['admissionCounters']['drains'] for r in values if r['admissionCounters']])}
paired['medianDifferencePercent'] = median([p['differencePercent'] for p in pairs])
historical = []
allocation_path = ROOT/'testdata/connectivity/webrtc/allocation-20261022/results.json'
for run in json.loads(allocation_path.read_text())['runs']:
    if run['label'] not in ['allocation-before','allocation-after'] or run['sizeMiB'] != 256 or run['blockMiB'] != 1: continue
    path = ROOT/run['report']
    if not path.exists(): continue
    report = json.loads(path.read_text())
    for index,row in enumerate(report['rounds']):
        receive = [r for r in row['receiveTimeline'] if r['end'] >= row['started'] and r['at'] < row['started']+row['elapsedMs']]
        gaps = [r['at']-receive[i-1]['end'] for i,r in enumerate(receive) if i > 0]
        port = row['peers'][0]['selected']['local']['port']
        snapshots = [socket(s,port) for s in report['socketSamples'][2*index:2*index+2]]
        historical.append({'label':run['label'],'report':run['report'],'repetition':index,
            'MBps':row['MBps'],'elapsedMs':row['elapsedMs'],'outputWriteMs':row['outputWriteMs'],
            'firstWriteMs':row['firstWriteMs'],'receiveGapMaxMs':max(gaps,default=0),
            'socketDrops':snapshots[-1].get('d',0)-snapshots[0].get('d',0)})
result = {'date':'2026-10-22','groups':groups,'paired':paired,'pairs':pairs,'stages':stages,'runs':runs,'historical':historical,
          'method':'Same 256 MiB fixture, 1 MiB blocks, frozen browser commits, same sender binary, fresh browser per transfer; paired AB/BA order. Bootstrap resamples adjacent pair log ratios with fixed seed; interval describes this sample and is not an equivalence proof.'}
OUT.write_text(json.dumps(result,indent=2)+'\n',encoding='utf-8')
print(json.dumps({'groups':groups,'paired':paired,'stages':stages},indent=2))
