# #region DEBUG
import importlib.util, json, pathlib, shutil, stat, subprocess, sys

from build_support import ROOT, module_file, prepare_lab, upstream
HERE = pathlib.Path(__file__).resolve().parent
REPAIR = '--repair-rack' in sys.argv
BUILD = ROOT / ('.tmp/webrtc-stall-20261022/native-rack-repair' if REPAIR else '.tmp/webrtc-stall-20261022/native')
PREVIOUS = ROOT / '.tmp/webrtc-slowdown-20261022/native-stats'
BUILD.mkdir(parents=True, exist_ok=True)
relay = prepare_lab()
subprocess.run(['node', str(HERE.parent/'slowdown-20261022/prepare-native-stats.mjs'), str(upstream('github.com/pion/sctp@v1.11.1'))], cwd=ROOT, check=True)

def replace(source, before, after):
    if source.count(before) != 1:
        raise RuntimeError('Instrumentation contract changed: ' + before)
    return source.replace(before, after)

replacements = {}
for package, module in [('sctp','github.com/pion/sctp@v1.11.1'), ('dtls','github.com/pion/dtls/v3@v3.1.8')]:
    original = upstream(module)
    target = BUILD / package
    if not target.exists():
        shutil.copytree(original, target)
    for copied in target.rglob('*'):
        if copied.is_file(): copied.chmod(stat.S_IREAD | stat.S_IWRITE)
    (target / 'research_debug.go').write_text((HERE / 'trace.go.txt').read_text().replace('PACKAGE', package))
    source = (original / ('association.go' if package == 'sctp' else 'conn.go')).read_text()
    if package == 'sctp':
        if REPAIR:
            spec=importlib.util.spec_from_file_location('rack_repair',HERE/'rack-repair.py')
            repair=importlib.util.module_from_spec(spec)
            spec.loader.exec_module(repair)
            source=repair.apply(source)
        shutil.copyfile(HERE/'rack_retransmission_test.go.txt',target/'research_retransmission_test.go')
        source = replace(source, 'func (a *Association) readLoop() {', 'func (a *Association) readLoop() {\n// #region DEBUG\n defer researchFlush()\n// #endregion DEBUG')
        source = replace(source, '\ta.stats.incSACKsReceived()', '\ta.stats.incSACKsReceived()\n// #region DEBUG\n defer a.researchState(10)\n// #endregion DEBUG')
        source = replace(source, '\t\ta.stats.incT3Timeouts()', '\t\ta.stats.incT3Timeouts()\n// #region DEBUG\n a.researchState(11)\n a.researchOutstanding()\n// #endregion DEBUG')
        source = replace(source, '\treturn rawPackets, ok\n}\n\nfunc checkPacket', '\n// #region DEBUG\n if len(rawPackets)>0 { a.researchState(12); researchRecord(13,uint64(len(rawPackets))) }\n// #endregion DEBUG\n\treturn rawPackets, ok\n}\n\nfunc checkPacket')
        source += '''
// #region DEBUG
func (a *Association) researchState(kind uint64) {
 researchRecord(kind, uint64(a.cumulativeTSNAckPoint), uint64(a.CWND()), uint64(a.RWND()),
  uint64(a.inflightQueue.getNumBytes()), uint64(a.pendingQueue.getNumBytes()),
  uint64(a.myNextTSN), uint64(a.rtoMgr.getRTO()*1000), a.stats.getNumT3Timeouts())
}
func (a *Association) researchOutstanding() {
 for tsn:=a.cumulativeTSNAckPoint+1; tsn!=a.myNextTSN; tsn++ {
  item,ok:=a.inflightQueue.get(tsn)
  if !ok { continue }
  flags:=uint64(0); if item.acked {flags|=1}; if item.retransmit {flags|=2}
  researchRecord(14,uint64(tsn),uint64(item.nSent),uint64(item.missIndicator),flags,uint64(item.since.UnixMicro()),uint64(len(item.userData)))
 }
}
// #endregion DEBUG
'''
        stats = (original / 'association_stats.go').read_text()
        stats += '''
// #region DEBUG
func (a *Association) ResearchLossStats() map[string]uint64 {
 return map[string]uint64{"packetsSent":a.stats.getNumPacketsSent(),"packetsReceived":a.stats.getNumPacketsReceived(),"fastRetransmissions":a.stats.getNumFastRetrans(),"t3Timeouts":a.stats.getNumT3Timeouts(),"ackTimeouts":a.stats.getNumAckTimeouts()}
}
// #endregion DEBUG
'''
        (target / 'association_stats.go').write_text(stats)
        (target / 'association.go').write_text(source)
    else:
        source = replace(source, 'func (c *Conn) Close() error {', 'func (c *Conn) Close() error {\n// #region DEBUG\n defer researchFlush()\n// #endregion DEBUG')
        source = replace(source, '\tpkt.record.Header.SequenceNumber = seq', '\tpkt.record.Header.SequenceNumber = seq\n// #region DEBUG\n if data,ok:=pkt.record.Content.(*protocol.ApplicationData); ok { researchPacket(1,epoch,seq,data.Data); researchPlanDrop(epoch,seq,data.Data) }\n// #endregion DEBUG')
        source = replace(source, '\tcase *protocol.ApplicationData:\n', '\tcase *protocol.ApplicationData:\n// #region DEBUG\n researchPacket(2,header.Epoch,header.SequenceNumber,content.Data)\n// #endregion DEBUG\n')
        source = replace(source, '\tfor _, compactedRawPacket := range compactedRawPackets {', '\tfor _, compactedRawPacket := range compactedRawPackets {\n// #region DEBUG\n researchStarted := time.Now()\n if researchDropWire(compactedRawPacket) {continue}\n// #endregion DEBUG')
        source = replace(source, '\t\t\treturn netError(err)\n\t\t}\n\t}\n\n\treturn nil\n}', '\t\t\treturn netError(err)\n\t\t}\n// #region DEBUG\n if len(compactedRawPacket)>=13 { var sequence uint64; for _,b:=range compactedRawPacket[5:11] { sequence=sequence<<8|uint64(b) }; researchRecord(4,uint64(compactedRawPacket[3])<<8|uint64(compactedRawPacket[4]),sequence,uint64(len(compactedRawPacket)),uint64(time.Since(researchStarted).Microseconds()),uint64(researchStarted.UnixMicro())) }\n// #endregion DEBUG\n\t}\n\n\treturn nil\n}')
        (target / 'conn.go').write_text(source)

for filename, original in [('connection.go',ROOT/'transport/webrtc/provider/connection.go'), ('sctptransport.go',ROOT/'third_party/pion/webrtc/sctptransport.go')]:
    destination = BUILD / filename
    shutil.copyfile(PREVIOUS / filename, destination)
    replacements[str(original)] = str(destination)
module = module_file(BUILD, {'github.com/pion/sctp': BUILD/'sctp', 'github.com/pion/dtls/v3': BUILD/'dtls'})
(BUILD/'overlay.json').write_text(json.dumps({'Replace':replacements},indent=2))
shutil.copyfile(relay, BUILD/'relay-native.exe')
subprocess.run(['go','build','-modfile='+str(module),'-overlay='+str(BUILD/'overlay.json'),'-o',str(BUILD/'wind.exe'),'./cmd/wind'],cwd=ROOT,check=True)
# #endregion DEBUG
