import {readFileSync,writeFileSync,readdirSync,existsSync} from 'node:fs'
import {join,relative,resolve} from 'node:path'
import {createHash} from 'node:crypto'
const root=resolve('.tmp/webrtc-slowdown-20261022'),destination=resolve('testdata/connectivity/webrtc/slowdown-20261022')
const hash=file=>createHash('sha256').update(readFileSync(file)).digest('hex')
const data=JSON.parse(readFileSync(join(root,'analysis.json'),'utf8'))
const native=JSON.parse(readFileSync(join(root,'native-summary.json'),'utf8'))
const endpoints=JSON.parse(readFileSync(join(root,'endpoint-summary.json'),'utf8'))
const compactPath=p=>({local:p.local?.address+':'+p.local?.port,remote:p.remote?.address+':'+p.remote?.port,protocol:p.local?.protocol,rtt:p.pair?.currentRoundTripTime})
const runs=data.full.map(row=>{
 const report=JSON.parse(readFileSync(row.report,'utf8')),round=report.rounds[row.repetition]
 return {kind:'complete-file',run:row.runId,repetition:row.repetition,label:report.label,variant:report.variant,sourceBytes:row.bytes,sha256:row.sha256,MBps:row.MBps,elapsedMs:row.elapsedMs,firstWriteMs:row.firstWriteMs,outputWriteMs:row.outputWriteMs,sink:row.sink,trace:row.trace,externalDNSBlocked:row.externalDNSBlocked,bufferMode:row.bufferMode,nativeObservation:row.binaryDirectory.includes('native-stats'),netlog:report.netlogEnabled??false,interaction:report.interaction,blockMiB:report.blockMiB,previewMs:round.interactive.completedMs-round.interactive.startedMs||undefined,paths:[compactPath(round.initialPeers[0]),compactPath(round.peers[0].selected)],kernelReceiveBufferBytes:row.sockets.at(-1).rb,socketDrops:row.sockets.at(-1).drops-row.sockets[0].drops,udpCounters:row.udpDelta,tcpControlsAroundBatch:row.tcp,outputControlMBps:row.outputControl,native:native.find(x=>x.run===row.runId&&x.repetition===row.repetition),endpointSampling:endpoints.find(x=>x.run===row.runId&&x.repetition===row.repetition),raw:relative(resolve('.'),row.report).replaceAll('\\','/'),rawSHA256:hash(row.report),cleanup:report.remoteCleanup?.status}
})
for(const row of data.raw)runs.push({kind:'cached-sealed-records',run:row.id,sourceBytes:row.sourceBytes,verifiedMBps:row.verifiedMBps,bufferMode:row.bufferMode,kernelReceiveBufferBytes:row.sockets.at(-1)?.rb,socketDrops:row.sockets.at(-1)?.drops,selectedNativePair:row.native.pair,finalSctp:row.native.sctp,raw:'.tmp/tcp-quic-20261021/'+row.file,rawSHA256:hash('.tmp/tcp-quic-20261021/'+row.file)})
writeFileSync(join(destination,'runs.ndjson'),runs.map(row=>JSON.stringify(row)).join('\n')+'\n')
const median=values=>{values=[...values].sort((a,b)=>a-b);const m=Math.floor(values.length/2);return values.length%2?values[m]:(values[m-1]+values[m])/2}
const groups={}
for(const label of ['bulk-clean','preview-clean'])for(const variant of ['baseline','batch']){
 const rows=runs.filter(row=>row.label===label&&row.variant===variant)
 if(!rows.length)continue
 groups[label+'-'+variant]={count:rows.length,MBps:rows.map(x=>x.MBps),medianMBps:median(rows.map(x=>x.MBps)),previewMs:rows.map(x=>x.previewMs).filter(x=>x!==undefined),medianPreviewMs:label==='preview-clean'?median(rows.map(x=>x.previewMs)):undefined,runs:rows.map(x=>x.run+':'+x.repetition)}
}
const netlogs=[]
for(const name of readdirSync(root)){const path=join(root,name,'report.json');if(!existsSync(path))continue;const report=JSON.parse(readFileSync(path,'utf8'));if(report.netlog)netlogs.push({run:name,...report.netlog})}
writeFileSync(join(destination,'background-network.json'),JSON.stringify(netlogs,null,2))
const selectRounds=suffix=>runs.filter(row=>row.run.endsWith(suffix)).map(row=>({run:row.run,repetition:row.repetition,MBps:row.MBps,outputWriteMs:row.outputWriteMs,kernelReceiveBufferBytes:row.kernelReceiveBufferBytes,socketDrops:row.socketDrops}))
const findings={
 currentProduction:{note:'The existing recvonly-video optimization remains effective. Complete-file performance can reach the earlier ~20 MB/s; output writes do not explain the repeated slow second round.',rounds:selectRounds('1792645555724')},
 backgroundTraffic:{note:'NetLog source dependencies associate ~37 MB of external QUIC receive traffic with Chrome optimization-guide model downloads during file transfer. External-DNS blocking in the owned test Chrome removes the recurring second-round dip in these controls. This does not isolate all system traffic or explain every slow round.',host:'optimizationguide-pa.googleapis.com',attributionRun:'slow-opfs-quiet-baseline-112.714714050293-0-none-1-lan-isolated-1792647179849',socketSource:'2130',quicSource:'2131',receivedBytes:37347401,normalDNS:selectRounds('1792646131331'),externalDNSBlocked:selectRounds('1792646174509')},
 receiveBuffer:{note:'Larger diagnostic receive buffers eliminate observed socket drops but not all throughput variation; there are slow rounds without drops, fast retransmissions or T3 expiry. The shim is not a product fix.',rounds:selectRounds('1792645703645')},
 batching:{note:'Three interleaved pairs per workflow with external DNS blocked do not establish a stable batch-64 throughput gain or the previously claimed preview regression. Both variants still have seconds of preview latency. DNS control is not full network isolation.',bulkOrder:['baseline','batch','batch','baseline','baseline','batch'],previewOrder:['baseline','batch','batch','baseline','baseline','batch']},
 unresolved:'Remaining variation after DNS control, including occasional delayed first output, is not fully attributed. No claim of constant 20 MB/s, production-code regression, or WAN performance.'
}
const overview={findings,date:'2026-10-22',head:'49f9919e136858ca0e4e5f9a9dc655c2dde48f7a',productionOptimizationCommit:'50892293a2062efe1c96c1731ff48c97ef05a205',fullFileRounds:runs.filter(x=>x.kind==='complete-file').length,cachedRecordRounds:data.raw.length,groups,binaries:Object.fromEntries(['baseline','batch'].map(variant=>[variant,hash('.tmp/webrtc-plan-revision-20261022/'+variant+'/wind.exe')])),currentNativeDiagnosticBinarySHA256:hash('.tmp/webrtc-slowdown-20261022/native-stats/wind.exe'),scope:'Opt-in lab research. Complete files include native read/seal, production protocol, browser authentication and output close; exclude setup and post-close SHA-256. TCP controls bracket each browser batch. OS caches and OPFS close are not crash-durability evidence. Diagnostic native overlays only sample statistics. External DNS isolation affects only the owned test Chrome; WebRTC remains direct IPv4 LAN.',baselineCaveat:'Original plan-revision throughput and preview comparisons did not control Chrome background traffic and do not establish fixed gains or regressions.',runs:'runs.ndjson',backgroundNetwork:'background-network.json'}
writeFileSync(join(destination,'results.json'),JSON.stringify(overview,null,2))
console.log(JSON.stringify(overview,null,2))
