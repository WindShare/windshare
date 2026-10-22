import {readFileSync,readdirSync,existsSync,writeFileSync} from 'node:fs'
import {join} from 'node:path'
const root='.tmp/webrtc-slowdown-20261022',results=[]
for(const name of readdirSync(root)){
 const path=join(root,name,'native-stats.ndjson'), reportPath=join(root,name,'report.json')
 if(!existsSync(path)||!existsSync(reportPath))continue
 const report=JSON.parse(readFileSync(reportPath,'utf8'))
 const all=readFileSync(path,'utf8').trim().split('\n').map(JSON.parse)
 for(const row of report.rounds??[]){
  const port=row.peers[0].selected.local.port
  const owned=all.filter(sample=>sample.pair?.Remote.port===port)
  const samples=owned.filter(sample=>sample.sctp.bytesSent>10000&&sample.sctp.bytesSent<row.size*1.02)
  const first=samples[0]?.time??0
  const reductions=samples.slice(1).flatMap((row,index)=>{
   const prev=samples[index],a=prev.sctp.congestionWindow,b=row.sctp.congestionWindow
   return b<a*.75?[{afterMs:row.time-first,from:a,to:b,buffered:row.buffered,bytesSent:row.sctp.bytesSent,rtt:row.sctp.smoothedRoundTripTime,loss:row.loss}]:[]
  })
  const pending=samples.filter(row=>row.buffered>65536).length
  results.push({run:name,repetition:row.repetition,MBps:row.MBps,buffer:report.remote.bufferMode,firstWriteMs:row.firstWriteMs,samples:samples.length,samplesWithQueuedSend:pending,reductions,maxRtt:Math.max(...samples.map(x=>x.sctp.smoothedRoundTripTime)),minReceiverWindow:Math.min(...samples.map(x=>x.sctp.receiverWindow)),paths:[...new Set(samples.map(x=>JSON.stringify(x.pair)))].length,loss:owned.at(-1)?.loss})
 }
}
writeFileSync(join(root,'native-summary.json'),JSON.stringify(results,null,2))
results.forEach(row=>console.log(JSON.stringify(row)))
