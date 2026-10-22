# #region DEBUG
import collections, json, pathlib, statistics, struct

ROOT=pathlib.Path(__file__).resolve().parents[4]
RUNS=ROOT/'.tmp/webrtc-stall-20261022/runs'
def lines(path):
    if not path.exists(): return []
    return [json.loads(line) for line in path.read_text().splitlines() if line]
def grouped(values,key):
    result=collections.defaultdict(list)
    for value in values: result[key(value)].append(value)
    return result
results=[]
for path in sorted(RUNS.glob('*/report.json')):
    report=json.loads(path.read_text())
    result={'report':path.relative_to(ROOT).as_posix(),'label':report.get('label'),'status':report.get('status'),'error':report.get('error'),'experiment':report.get('packetExperiment')}
    results.append(result)
    if not report.get('rounds'): continue
    row=report['rounds'][0]
    port=row['peers'][0]['selected']['local']['port']
    origin=(row['timeOrigin']+row['started'])*1000
    result.update(MBps=row['MBps'],firstWriteMs=row['firstWriteMs'],sha256=row['sha256'],selectedPort=port,cleanup=report.get('remoteCleanup'))
    native=[r for r in lines(path.parent/'native-stats.ndjson') if r.get('sctp') and r.get('loss')]
    result['timeoutsCounter']=max((r['loss']['t3Timeouts'] for r in native),default=None)
    result['fastRetransmissions']=max((r['loss']['fastRetransmissions'] for r in native),default=None)
    result['queuedZeroProgressSamples']=sum(b['sctp']['bytesSent']==a['sctp']['bytesSent'] and b['buffered']>0 for a,b in zip(native,native[1:])) if native else None
    drops=[]; previous={}
    for sample in report.get('udpTimeline',[]):
        for socket in sample.get('sockets',[]):
            if socket['port']!=port: continue
            key=(socket['family'],socket['inode'])
            prior=previous.get(key)
            if prior and socket['drops']>prior['drops']:
                drops.append({'fromRemoteUs':prior['at'],'toRemoteUs':sample['time']*1000,'count':socket['drops']-prior['drops'],'rxBefore':prior['rxBytes'],'rxAfter':socket['rxBytes']})
            previous[key]={**socket,'at':sample['time']*1000}
    result['kernelDrops']=sum(d['count'] for d in drops)
    result['kernelDropWindows']=drops
    dtls=lines(path.parent/'sender.dtls.ndjson')
    sctp=lines(path.parent/'sender.sctp.ndjson')
    result['senderTraceMetadata']=[r for r in dtls+sctp if isinstance(r,dict)]
    dtls=[r for r in dtls if isinstance(r,list)]
    sctp=[r for r in sctp if isinstance(r,list)]
    data=[r for r in dtls if r[1]==1 and r[4] in (0,64)]
    tx=grouped(data,lambda r:r[5])
    wire={tuple(r[2:4]):r for r in dtls if r[1]==4}
    acks=[r for r in dtls if r[1]==2 and r[4]==3]
    result['dataChunks']=len(data) if dtls else None
    result['repeatedDataChunks']=len(data)-len(tx) if dtls else None
    injections=[r for r in dtls if r[1]==5]
    result['injectedDrops']=injections
    result['injectedRecovery']=[{'tsn':tsn,'attempts':versions,'retryRecoveryMs':(versions[2][0]-versions[1][0])/1000 if len(versions)>2 else None} for tsn in sorted(set(r[4] for r in injections)) for versions in [tx.get(tsn,[])]]
    result['injectedRetryRecoveryMs']=next((r['retryRecoveryMs'] for r in result['injectedRecovery']),None)
    receiver_path=path.parent/'receiver.bin'
    received={}; sent={}
    if receiver_path.exists():
        raw=receiver_path.read_bytes()
        header=struct.unpack('<8Q',raw[:64])
        receiver=list(struct.iter_unpack('<QQQIIiHHIIQQ',raw[64:]))
        flows=grouped([r for r in receiver if r[3]==1],lambda r:(r[4],r[5]))
        flow_scores={key:sum((r[6],r[2]) in wire and wire[(r[6],r[2])][4]==r[8] for r in rows) for key,rows in flows.items()}
        selected_flow=max(flow_scores,key=flow_scores.get) if flow_scores else None
        result['receiverFlow']={'selected':selected_flow,'matching':flow_scores.get(selected_flow,0),'alternatives':len(flow_scores)}
        relevant=[r for r in receiver if (r[4],r[5])==selected_flow]
        received={(r[6],r[2]):r for r in relevant if r[3]==1}
        sent={(r[6],r[2]):r for r in relevant if r[3]==2}
        result['receiverTrace']={'header':header,'records':len(receiver),'selectedRecords':len(relevant),'incoming':len(received),'outgoing':len(sent),'ancillaryDrops':max((r[9] for r in relevant if r[9]!=4294967295),default=None),'pauseEvents':[r for r in receiver if r[3]==9]}
        upper=[r[0]/1000-wire[key][6] for key,r in received.items() if key in wire]
        lower=[sent[tuple(r[2:4])][0]/1000-r[0] for r in acks if tuple(r[2:4]) in sent]
        if lower and upper:
            result['packetClockOffsetBoundsUs']=[max(lower),min(upper)]
        missing={tuple(r[2:4]):r for r in data if tuple(r[2:4]) not in received}
        result['unreceivedDataRecords']=len(missing)
        inbound_keys=set(tuple(r[2:4]) for r in dtls if r[1]==2)
        result['unreceivedReverseRecords']=len(set(sent)-inbound_keys)
        clusters=[]
        for key,r in sorted(missing.items()):
            event={'sequence':key[1],'tsn':r[5],'sentUs':wire.get(key,r)[0]}
            if clusters and event['sequence']==clusters[-1]['lastSequence']+1:
                clusters[-1].update(lastSequence=event['sequence'],lastSentUs=event['sentUs'],count=clusters[-1]['count']+1)
            else: clusters.append({'firstSequence':event['sequence'],'lastSequence':event['sequence'],'firstSentUs':event['sentUs'],'lastSentUs':event['sentUs'],'count':1})
        result['missingBursts']=clusters
    timeouts=[]
    for index,r in enumerate(sctp):
        if r[1]!=11: continue
        outstanding=[]
        for item in sctp[index+1:]:
            if item[1]!=14: break
            if item[5]&1: continue
            versions=[v for v in tx.get(item[2],[]) if v[0]<=r[0]]
            versions=[{'atUs':v[0],'epoch':v[2],'sequence':v[3],'received':tuple(v[2:4]) in received,'receiverUs':received[tuple(v[2:4])][0]/1000 if tuple(v[2:4]) in received else None} for v in versions]
            outstanding.append({'tsn':item[2],'sendCount':item[3],'missIndicator':item[4],'flags':item[5],'sinceUs':item[6],'bytes':item[7],'transmissions':versions})
        prior=[ack for ack in acks if ack[0]<r[0]]
        timeouts.append({'atUs':r[0],'state':r[2:],'unackedCount':len(outstanding),'outstanding':outstanding,'lastAck':prior[-1] if prior else None})
    result['timeouts']=timeouts
    result['maxGatherPackets']=max((r[2] for r in sctp if r[1]==13),default=None)
    result['tcpMBps']=[r['MBps'] for r in report.get('tcpControls',[])]
destination=ROOT/'.tmp/webrtc-stall-20261022/analysis.json'
destination.write_text(json.dumps({'runs':results},indent=2))
compact=[{k:r.get(k) for k in ['label','status','MBps','firstWriteMs','timeoutsCounter','fastRetransmissions','kernelDrops','unreceivedDataRecords','unreceivedReverseRecords','repeatedDataChunks','packetClockOffsetBoundsUs','maxGatherPackets','injectedRetryRecoveryMs']} for r in results]
(ROOT/'.tmp/webrtc-stall-20261022/compact.json').write_text(json.dumps(compact,indent=2))
with (ROOT/'.agents/debug.log').open('a') as log:
    log.write(json.dumps({'event':'[DEBUG H1 H2 H3 H4] analyzed','path':str(destination),'runs':len(results)})+'\n')
# #endregion DEBUG
