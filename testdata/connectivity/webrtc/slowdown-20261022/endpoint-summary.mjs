import {readFileSync,readdirSync,existsSync,writeFileSync} from 'node:fs'
import {join} from 'node:path'
const root='.tmp/webrtc-slowdown-20261022',results=[]
for(const name of readdirSync(root)){
 const path=join(root,name,'report.json');if(!existsSync(path))continue
 const r=JSON.parse(readFileSync(path,'utf8'))
 const nativePath=join(root,name,'native-stats.ndjson')
 if(!existsSync(nativePath))continue
 const native=readFileSync(nativePath,'utf8').trim().split('\n').map(JSON.parse)
 for(const [index,row] of r.rounds.entries()){
  if(!row.browserProcesses)continue
  const samples=native.filter(sample=>sample.pair?.Remote.port===row.peers[0].selected.local.port&&sample.sctp.bytesSent>10000&&sample.sctp.bytesSent<row.size*1.02)
  const first=samples[0],last=samples.at(-1),cpu=first?.cpu&&last?.cpu?{seconds:(last.time-first.time)/1000,kernel:last.cpu.kernel-first.cpu.kernel,user:last.cpu.user-first.cpu.user}:null
  results.push({run:name,repetition:index,MBps:row.MBps,elapsedMs:row.elapsedMs,writesMs:row.outputWriteMs,nativeCPU:cpu,browserCPU:row.browserProcesses.filter(x=>x.cpuSeconds>.01),browserMetrics:row.cpuSeconds,wifi:r.socketSamples.slice(index*2,index*2+2).map(x=>({phase:x.phase,value:x.wifi}))})
 }
}
writeFileSync(join(root,'endpoint-summary.json'),JSON.stringify(results,null,2))
console.log(JSON.stringify(results,null,2))
