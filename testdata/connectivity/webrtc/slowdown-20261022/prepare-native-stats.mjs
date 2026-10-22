import {readFileSync,writeFileSync,mkdirSync,copyFileSync,cpSync,existsSync} from 'node:fs'
import {resolve,join} from 'node:path'
const root=resolve('.tmp/webrtc-slowdown-20261022/native-stats')
mkdirSync(root,{recursive:true})
const path=resolve('transport/webrtc/provider/connection.go')
let source=readFileSync(path,'utf8').replace('import (','import (\n "os"\n "encoding/json"\n "golang.org/x/sys/windows"')
source=source.replace('connection.PeerConnection = pc','connection.PeerConnection = pc\n researchObserveSCTP(pc)')
source+=`
func researchCPUSeconds() map[string]float64 {
 var creation, exit, kernel, user windows.Filetime
 _ = windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user)
 seconds := func(value windows.Filetime) float64 { return float64(uint64(value.HighDateTime)<<32|uint64(value.LowDateTime))/1e7 }
 return map[string]float64{"kernel":seconds(kernel),"user":seconds(user)}
}
func researchObserveSCTP(pc *pion.PeerConnection) {
 path := os.Getenv("WS_RESEARCH_PION_STATS")
 if path == "" { return }
 go func() {
  output, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
  if err != nil { return }
  defer output.Close()
  encoder := json.NewEncoder(output)
  ticks := time.NewTicker(200*time.Millisecond)
  defer ticks.Stop()
  for range ticks.C {
   if pc.ConnectionState() == pion.PeerConnectionStateClosed { return }
   transport := pc.SCTP()
   if transport == nil { continue }
   pair, _ := transport.Transport().ICETransport().GetSelectedCandidatePair()
   _ = encoder.Encode(map[string]any{"time":time.Now().UnixMilli(),"cpu":researchCPUSeconds(),"state":pc.ConnectionState().String(),"sctp":transport.Stats(),"loss":transport.ResearchLossStats(),"buffered":transport.BufferedAmount(),"pair":pair})
  }
 }()
}
`
const overlayPath=join(root,'connection.go')
writeFileSync(overlayPath,source)
const sctpModule=resolve(process.argv[2]??'C:/Users/lab/go/pkg/mod/github.com/pion/sctp@v1.11.1')
const sctpCopy=join(root,'sctp')
if(!existsSync(sctpCopy))cpSync(sctpModule,sctpCopy,{recursive:true})
const sctpPath=join(sctpCopy,'association_stats.go')
let mod=readFileSync('go.mod','utf8')
for(const dependency of ['ice','webrtc','turn'])mod=mod.replace('./third_party/pion/'+dependency,JSON.stringify(resolve('third_party/pion/'+dependency).replaceAll('\\','/')))
mod+='\nreplace github.com/pion/sctp => '+JSON.stringify(sctpCopy.replaceAll('\\','/'))+'\n'
writeFileSync(join(root,'diag.mod'),mod)
copyFileSync('go.sum',join(root,'diag.sum'))
const sctpOverlay=join(root,'association_stats.go'),pionPath=resolve('third_party/pion/webrtc/sctptransport.go'),pionOverlay=join(root,'sctptransport.go')
writeFileSync(sctpOverlay,readFileSync(sctpPath,'utf8')+`
func (a *Association) ResearchLossStats() map[string]uint64 {
 return map[string]uint64{"packetsSent":a.stats.getNumPacketsSent(),"packetsReceived":a.stats.getNumPacketsReceived(),"fastRetransmissions":a.stats.getNumFastRetrans(),"t3Timeouts":a.stats.getNumT3Timeouts(),"ackTimeouts":a.stats.getNumAckTimeouts()}
}
`)
writeFileSync(pionOverlay,readFileSync(pionPath,'utf8')+`
func (r *SCTPTransport) ResearchLossStats() map[string]uint64 {
 association := r.association()
 if association == nil { return nil }
 return association.ResearchLossStats()
}
`)
writeFileSync(join(root,'overlay.json'),JSON.stringify({Replace:{[path]:overlayPath,[sctpPath]:sctpOverlay,[pionPath]:pionOverlay}},null,2))
copyFileSync('.tmp/webrtc-plan-revision-20261022/baseline/relay-native.exe',join(root,'relay-native.exe'))
console.log(join(root,'overlay.json'))
