"""Retain every attempted run and compare the fixed 20-80% output interval."""
import hashlib
import json
import os
from pathlib import Path
import statistics
import sys

ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from evidence_export import write_public
from experiment_paths import experiment_directory
BUILD = experiment_directory(ROOT, os.environ.get('WS_ADAPTIVE_BUILD', 'webrtc-sampling-stress-20261024'))


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def summarize(round):
    total = round['size']
    points = [(round['started'], 0)]
    written = 0
    for write in round['writeTimeline']:
        written += write['bytes']
        points.append((write['end'], written))
    def time_at(size):
        for index, (at, done) in enumerate(points[1:], 1):
            if done >= size:
                before, previous = points[index-1]
                return before + (at-before)*(size-previous)/(done-previous)
        raise ValueError('Incomplete output timeline')
    demand = round.get('demandTimeline', [])
    decisions = [event for event in round.get('protocolTraces', []) if event.get('stage') == 'parallelism']
    evidence_errors = []
    for decision in decisions:
        work = decision.get('trialWork')
        if work is not None and any(value < 0 for value in work.values()):
            evidence_errors.append('negative sampling evidence')
        if work is not None and decision['reason'] in ['no-aggregate-gain','aggregate-progress-improved']:
            settled = sum(work[field] for field in ['completedAttempts','failedAttempts','supersededAttempts'])
            if settled < 4 or (work['busyMilliseconds'] < 1000 and work['completedBytes'] < 8*1048576):
                evidence_errors.append('decision without representative work')
    peak = max((entry['peers'] for entry in demand), default=0)
    if peak > 3: evidence_errors.append('throughput growth used the recovery slot')
    if round['errors']: evidence_errors.append('browser page errors')
    return dict(bytes=total, sha256=round['sha256'], overallMBps=total/round['activationToCompleteMs']/1000,
        middle20to80MBps=.6*total/(time_at(.8*total)-time_at(.2*total))/1000,
        completionMs=round['activationToCompleteMs'], firstOutputMs=round['activationToFirstWriteMs'],
        workerStatus=round['workerStatus'], lifecycle=round['lifecycle'],
        maximumObservedPaths=peak, createdPaths=len(round['peers']),
        maximumObservedInFlightBytes=max((entry['inFlightBytes'] for entry in demand), default=0),
        maximumObservedQueuedBytes=max((entry['queuedBytes'] for entry in demand), default=0),
        parallelismDecisions=decisions, interactive=round.get('interactive'), fault=round.get('fault'),
        archiveBytes=round.get('archiveBytes'), evidenceErrors=evidence_errors)


def main():
    runs=[]
    for file in sorted(BUILD.glob('*-ledger.json')):
        ledger=json.loads(file.read_text())
        for run in ledger['runs']:
            item=dict(run)
            item['suite']=ledger['suite']
            item['ledgerSHA256']=sha(file)
            if 'report' in item:
                report_file=ROOT/item['report']
                if sha(report_file)!=item['reportSHA256']: raise ValueError('Report identity changed: '+item['report'])
                report=json.loads(report_file.read_text())
                item['scope']=report['scope']
                item['browserVersion']=report.get('browserVersion')
                item['tcpControls']=report.get('tcpControls')
                item['remoteCleanup']=report.get('remoteCleanup')
                item['rounds']=[summarize(row) for row in report['rounds']]
            runs.append(item)
    groups={}
    for run in runs:
        if run['suite'] not in ['large','medium','medium-confirmation']: continue
        version='fixed-three' if run.get('paths')==3 else ('before' if run['name'].startswith('before') else 'current')
        key=str(run.get('sizeMiB',256))+'MiB-'+version
        for row in run.get('rounds',[]):groups.setdefault(key,[]).append(row)
    aggregate={key:dict(count=len(rows),overallMedianMBps=statistics.median(row['overallMBps'] for row in rows),
        middleMedianMBps=statistics.median(row['middle20to80MBps'] for row in rows),
        overallRangeMBps=[min(row['overallMBps'] for row in rows),max(row['overallMBps'] for row in rows)]) for key,rows in groups.items()}
    result=dict(runs=runs,aggregate=aggregate,attempted=len(runs),
        completed=sum(len(run.get('rounds',[])) for run in runs),
        failed=[run['label'] for run in runs if run.get('status')!='passed' or run.get('failure') or run.get('exitCode',0)!=0],
        evidenceErrors=[dict(label=run['label'],errors=row['evidenceErrors']) for run in runs for row in run.get('rounds',[]) if row['evidenceErrors']])
    (BUILD/'analysis.json').write_text(json.dumps(result,indent=2)+'\n')
    if '--archive' in sys.argv:write_public(Path(__file__).with_name('results.json'),result)
    print(json.dumps({key:result[key] for key in ['attempted','completed','failed','evidenceErrors','aggregate']},indent=2))
    if result['failed'] or result['evidenceErrors']: raise SystemExit(1)

if __name__=='__main__':main()
