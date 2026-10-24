"""Record native workflow integrity and the actual default ICE routes separately."""
from pathlib import Path
import hashlib
import json
import os
import sys
ROOT=Path(__file__).resolve().parents[4]
BUILD=ROOT/'.tmp'/os.environ.get('WS_SAMPLING_NATIVE_BUILD','webrtc-sampling-native-20261024')
sys.path.insert(0,str(Path(__file__).resolve().parent.parent))
from evidence_export import write_public

results=[]
for file in sorted(BUILD.glob('native-*/report.json')):
    report=json.loads(file.read_text())
    item={key:report.get(key) for key in ['direction','size','sha256','scope','status','remoteCleanup','windowsSHA256','linuxSHA256','error']}
    item['reportSHA256']=hashlib.sha256(file.read_bytes()).hexdigest()
    item['rounds']=[]
    for row in report['rounds']:
        receiver=file.parent/f"receiver-{row['repetition']-1}.ndjson"
        evidence=[json.loads(line) for line in receiver.read_text().splitlines()] if receiver.exists() else []
        pairs=[event['payload']['selected_pair'] for event in evidence if event.get('payload',{}).get('kind')=='selected_pair']
        decisions=[dict(elapsedMs=event['elapsed_ms'],**event['payload']['parallelism']) for event in evidence if event.get('payload',{}).get('kind')=='parallelism']
        item['rounds'].append(dict(repetition=row['repetition'],completionMs=row.get('completionMs'),MBpsIncludingStartup=row.get('MBpsIncludingStartup'),
            exitCode=row.get('exitCode'),sha256=row.get('sha256'),selectedPairs=pairs,parallelismDecisions=decisions,
            traceSHA256=hashlib.sha256(receiver.read_bytes()).hexdigest() if receiver.exists() else None))
    results.append(item)
write_public(Path(__file__).with_name('native-results.json'),results)
print(json.dumps([dict(direction=row['direction'],status=row['status'],rounds=[dict(rate=r['MBpsIncludingStartup'],exitCode=r['exitCode'],pairs=[p['local_type']+'/'+p['remote_type'] for p in r['selectedPairs']]) for r in row['rounds']]) for row in results],indent=2))
