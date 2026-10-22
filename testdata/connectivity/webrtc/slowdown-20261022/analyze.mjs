import { readdirSync, readFileSync, existsSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
const root = resolve('.tmp/webrtc-slowdown-20261022')
const receiverHost = process.env.WS_LAB_RECEIVER_HOST ?? '192.0.2.105'
function udp(text) {
  const rows = text.split('\n').filter(line=>line.startsWith('Udp:')).map(line=>line.split(/\s+/).slice(1))
  return Object.fromEntries(rows[0].map((name,index)=>[name,Number(rows[1][index])]))
}
function socket(text, port) {
  const lines = text.split('\n'), index = lines.findIndex(line=>line.includes(receiverHost+':'+port+' '))
  if(index<0) return null
  const memory = lines[index+1], rb = Number(memory.match(/\brb(\d+)/)?.[1]), drops = Number(memory.match(/\bd(\d+)/)?.[1])
  return {rb,drops,raw:lines.slice(index,index+2).join('\n')}
}
const results=[]
for(const directory of readdirSync(root)) {
 const path=join(root,directory,'report.json')
 if(!existsSync(path)) continue
 const report=JSON.parse(readFileSync(path,'utf8'))
 if(report.status!=='passed') continue
 for(const [index,row] of report.rounds.entries()) {
  const port=row.peers[0].selected.local.port, before=report.socketSamples[index*2],after=report.socketSamples[index*2+1]
  const start=udp(before.snmp),end=udp(after.snmp)
  results.push({runId:report.runId,repetition:index,report:path,binaryDirectory:report.binaryDirectory??'.tmp/webrtc-plan-revision-20261022/baseline',sink:report.sinkMode,trace:report.traceEnabled,bufferMode:report.remote.bufferMode??'default',externalDNSBlocked:report.remote.externalDNSBlocked??false,bytes:row.size,MBps:row.MBps,elapsedMs:row.elapsedMs,firstWriteMs:row.firstWriteMs,outputWriteMs:row.outputWriteMs,sha256:row.sha256,selected:row.peers[0].selected,cpu:row.cpuSeconds,tcp:report.tcpControls.map(x=>x.MBps),outputControl:report.outputControl.MBps,sockets:[socket(before.ss,port),socket(after.ss,port)],udpDelta:Object.fromEntries(Object.keys(start).map(name=>[name,end[name]-start[name]]))})
 }
}
const raw=[]
for(const name of readdirSync('.tmp/tcp-quic-20261021').filter(name=>name.startsWith('slowdown-')&&name.endsWith('.json'))) {
 const report=JSON.parse(readFileSync(join('.tmp/tcp-quic-20261021',name),'utf8'))
 for(const trial of report.trials??[]) {
  if(!trial.result?.facts?.selected)continue
  const port=trial.result.facts.selected.local.port
  const snapshots=report.remote.filter(row=>row.event==='snapshot'&&(row.id===trial.id+'-before'||row.id===trial.id+'-after'))
  raw.push({file:name,id:trial.id,sourceBytes:trial.result.facts.content?.verifiedBytes,bufferMode:report.remote.find(row=>row.event==='ready')?.bufferMode??'default',MBps:trial.result.MBps,verifiedMBps:trial.result.facts.content?.verifiedMBps,native:trial.result.facts.native,sockets:snapshots.map(row=>socket(row.sockets.udp,port))})
 }
}
writeFileSync(join(root,'analysis.json'),JSON.stringify({full:results,raw},null,2))
for(const row of results)console.log(JSON.stringify({file:row.runId,sink:row.sink,trace:row.trace,buffer:row.bufferMode,MBps:row.MBps,writeMs:row.outputWriteMs,rb:row.sockets[1]?.rb,drops:row.sockets[1]?.drops,udpDrops:row.udpDelta.RcvbufErrors,tcp:row.tcp}))
for(const row of raw)console.log(JSON.stringify({file:row.file,verifiedMBps:row.verifiedMBps,sockets:row.sockets,nativeSctp:row.native.sctp}))
