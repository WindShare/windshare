from pathlib import Path
import copy, json, random
rng = random.Random(20261024)
root = Path.cwd()
MiB = 1 << 20
fields = ['offeredBytes','settledBytes','completedBytes','completedAttempts','failedAttempts','supersededAttempts','cancelledAttempts','busyMilliseconds']
def path(lane):
    return {'identity':{'pathId':f'path-{lane}','laneId':lane,'laneEpoch':1},'optional':lane!=1,'work':dict.fromkeys(fields,0)}
cases = json.loads((root/'testdata/connectivity/webrtc/throughput-control.json').read_text())
for index in range(1500):
    paths = [path(1)]
    now = useful = output = 0
    samples = []
    for tick in range(60):
        dt = rng.choice([0, 100, 500, 1000, 1000, 1500, 3000, 5000])
        now += dt
        if tick in [4, 12, 30] and len(paths)<3:
            paths.append(path(max(p['identity']['laneId'] for p in paths)+1))
        if tick>5 and rng.random()<.06 and len(paths)>1:
            paths.pop()
        if tick>5 and rng.random()<.05:
            target=rng.choice(paths)
            target['identity']['laneEpoch']+=1
            target['work']=dict.fromkeys(fields,0)
        for item in paths:
            work=item['work']
            attempts=rng.randrange(12)
            cancelled=rng.randrange(attempts+1)
            failed=rng.randrange(attempts-cancelled+1)
            superseded=rng.randrange(attempts-cancelled-failed+1)
            completed=attempts-cancelled-failed-superseded
            work['offeredBytes']+=attempts*MiB
            work['settledBytes']+=attempts*MiB
            work['completedBytes']+=completed*MiB
            for field,value in [('completedAttempts',completed),('failedAttempts',failed),('supersededAttempts',superseded),('cancelledAttempts',cancelled)]:work[field]+=value
            work['busyMilliseconds']+=rng.randrange(dt+1) if attempts else 0
            if rng.random()<.02:item['work']=dict.fromkeys(fields,0)
        progress=rng.randrange(32)*MiB*dt//1000
        useful+=progress
        output=min(useful, output+int(progress*rng.choice([.4,.7,1.,1.,1.3])))
        if rng.random()<.015:useful=output=0
        busy=rng.random()>.08
        samples.append(dict(at=now,inFlightBytes=2*MiB if busy else 0,queuedBytes=8*MiB if busy else 0,networkWaitingBytes=8*MiB if busy else 0,unfinishedBytes=MiB if busy else 0,capacityWaitingBytes=MiB if rng.random()<.15 else 0,usefulBytes=useful,outputBytes=output,remainingBytes=rng.choice([4,64,1024])*MiB,remainingKnown=rng.choice([True,False]),paths=copy.deepcopy(paths),pending=rng.random()<.1))
    cases.append(dict(name=f'seed-20261024-{index}',maximumPaths=3,samples=samples))
(root/'.tmp/sampling-stress-tools').mkdir(parents=True,exist_ok=True)
(root/'.tmp/sampling-stress-tools/generated.json').write_text(json.dumps(cases,separators=(',',':')))
print(json.dumps({'scenarios':len(cases),'samples':sum(len(c['samples']) for c in cases),'seed':20261024}))
