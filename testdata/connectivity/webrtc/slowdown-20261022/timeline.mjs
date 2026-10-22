import {readFileSync,readdirSync,existsSync} from 'node:fs'
import {join} from 'node:path'
const root='.tmp/webrtc-slowdown-20261022'
for(const name of readdirSync(root)){
 const path=join(root,name,'report.json');if(!existsSync(path))continue
 const r=JSON.parse(readFileSync(path,'utf8'))
 for(const row of r.rounds??[]){
  if(!row.writeTimeline)continue
  const relative=values=>values.map(value=>({...value,at:Math.round(value.at-row.started),end:value.end===undefined?undefined:Math.round(value.end-row.started)}))
  const writes=relative(row.writeTimeline),recv=relative(row.receiveTimeline),dispatch=relative(row.dispatchTimeline)
  const gaps=values=>values.slice(1).map((v,i)=>({after:values[i],before:v,gap:v.at-(values[i].end??values[i].at)})).sort((a,b)=>b.gap-a.gap).slice(0,5)
  const quarters=[];let priorEnd=0,priorBytes=0,accumulated=0;for(const write of writes){accumulated+=write.bytes;if(accumulated-priorBytes>=64*1024*1024){quarters.push({endBytes:accumulated,endMs:write.end,MBps:(accumulated-priorBytes)/(write.end-priorEnd)/1000});priorEnd=write.end;priorBytes=accumulated}}
  console.log(JSON.stringify({run:name,MBps:row.MBps,firstWriteMs:row.firstWriteMs,quarters,receiveGaps:gaps(recv.filter(x=>x.at>=0)).map(x=>({after:x.after.end,before:x.before.at,gap:x.gap})),writeGaps:gaps(writes).map(x=>({after:x.after.index,before:x.before.index,gap:x.gap}))}))
 }
}
