import json, pathlib, collections, statistics

ROOT = pathlib.Path(__file__).resolve().parents[4]
RUNS = ROOT / '.tmp/webrtc-interleaved-20261022/runs'
results = []
for report_path in sorted(RUNS.glob('*/report.json')):
    report = json.loads(report_path.read_text(encoding='utf-8'))
    if not report.get('retest',{}).get('diagnostic') or report.get('status') != 'passed': continue
    profile_path = report_path.parent / '0-cpu-profile.json'
    profile = json.loads(profile_path.read_text())['profile']
    nodes = {n['id']:n for n in profile['nodes']}
    parents = {c:n['id'] for n in profile['nodes'] for c in n.get('children',[])}
    self_time, inclusive = collections.Counter(), collections.Counter()
    for sample,delta in zip(profile['samples'],profile['timeDeltas']):
        self_time[sample] += delta
        while sample:
            inclusive[sample] += delta
            sample = parents.get(sample)
    def describe(node, microseconds):
        frame = nodes[node]['callFrame']
        return {'function':frame['functionName'],'url':frame['url'],'line':frame['lineNumber']+1,'sampleMs':microseconds/1000}
    hot = [describe(n,t) for n,t in self_time.most_common(25)]
    sched = [describe(n,t) for n,t in inclusive.most_common() if '/src/content/scheduling/' in nodes[n]['callFrame']['url']][:20]
    row = report['rounds'][0]
    blocks = row['blockTimings']
    phases = {'requestToFirstMedianMs':statistics.median(b['firstReceived']-b['started'] for b in blocks),
              'lastFragmentToAuthMedianMs':statistics.median(b['authenticated']-b['lastReceived'] for b in blocks),
              'lastFragmentToAuthTotalMs':sum(b['authenticated']-b['lastReceived'] for b in blocks)}
    events = sorted([(b['firstReceived'],1) for b in blocks]+[(b['lastReceived'],-1) for b in blocks])
    busy, previous, gap = 0, None, []
    for when,delta in events:
        if previous is not None and busy == 0: gap.append({'from':previous-row['started'],'to':when-row['started'],'ms':when-previous})
        busy += delta
        previous = when
    phases['betweenRecordsIdleMs'] = sum(g['ms'] for g in gap)
    phases['largestBetweenRecordsGaps'] = sorted(gap,key=lambda g:g['ms'],reverse=True)[:6]
    native_path = report_path.parent / 'native-stats.ndjson'
    native = []
    if native_path.exists():
        samples = [json.loads(s) for s in native_path.read_text().splitlines()]
        started = row['timeOrigin'] + row['started']
        ended = started + row['elapsedMs']
        # Both hosts' wall clocks are sampled in the report; native samples are diagnostic,
        # and content byte progress is the portable alignment when clock offsets exist.
        progress = [s for s in samples if s.get('sctp',{}).get('bytesSent',0)>1024*1024 and s.get('sctp',{}).get('bytesSent',0)<row['size']]
        timeout_windows = []
        for index in range(1,len(samples)):
            current, prior = samples[index],samples[index-1]
            if (current.get('loss') or {}).get('t3Timeouts',0) <= (prior.get('loss') or {}).get('t3Timeouts',0): continue
            if not current.get('pair') or current['pair']['Remote']['port'] != row['peers'][0]['selected']['local']['port']: continue
            window = []
            for near in range(max(1,index-3),min(len(samples),index+3)):
                now, earlier = samples[near],samples[near-1]
                window.append({'time':now['time'],'deltaMs':now['time']-earlier['time'],
                    'sentBytes':now['sctp']['bytesSent']-earlier['sctp']['bytesSent'],
                    'buffered':now['buffered'],'cwnd':now['sctp']['congestionWindow'],
                    't3Timeouts':now['loss']['t3Timeouts']})
            timeout_windows.append(window)
        samples = progress
        if samples:
            a,b = samples[0], samples[-1]
            native = {'first':a,'last':b,'sampleCount':len(samples),'timeoutWindows':timeout_windows,
                      'bufferedMin':min(s['buffered'] for s in samples),'bufferedMedian':statistics.median(s['buffered'] for s in samples),
                      'zeroBufferedSamples':sum(s['buffered']==0 for s in samples),
                      'cwndMin':min(s['sctp'].get('congestionWindow',0) for s in samples),
                      'cwndMedian':statistics.median(s['sctp'].get('congestionWindow',0) for s in samples),
                      'rttMedian':statistics.median(s['sctp'].get('smoothedRoundTripTime',0) for s in samples)}
    results.append({'arm':report['retest']['arm'],'order':report['retest']['order'],
        'report':str(report_path.relative_to(ROOT)).replace('\\','/'),'MBps':row['MBps'],
        'phases':phases,'hotSelf':hot,'schedulingInclusive':sched,'native':native})
out = ROOT/'testdata/connectivity/webrtc/interleaved-20261022/profiles.json'
out.write_text(json.dumps(results,indent=2)+'\n',encoding='utf-8')
for r in results:
    print(json.dumps({k:r[k] for k in ['arm','order','MBps','phases','hotSelf','schedulingInclusive']},indent=2))
