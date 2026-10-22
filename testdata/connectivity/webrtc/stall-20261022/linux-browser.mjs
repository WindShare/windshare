// #region DEBUG
import assert from 'node:assert/strict'
export {lab,sshArgs} from '../slowdown-20261022/linux-browser.mjs'
import { execFileSync } from 'node:child_process'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import { join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
const root = fileURLToPath(new URL('../../../../', import.meta.url))
const frozen = (revision,path) => execFileSync('git',['show',revision+':testdata/connectivity/webrtc/'+path],{cwd:root,encoding:'utf8',windowsHide:true})
const replace = (source,before,after) => { assert.equal(source.split(before).length,2,'Receiver probe contract: '+before); return source.replace(before,after) }
export async function startLinuxBrowser(ports,evidence) {
 const output = process.env.WS_STALL_OUTPUT
 const directory = join(output,'harness')
 await mkdir(directory,{recursive:true})
 let python = frozen('f6d058cd','slowdown-20261022/lan-browser.py')
 const packets = process.env.WS_STALL_PACKET_TRACE === '1'
 if(packets) {
  const library = await readFile(new URL('./receiver.c',import.meta.url),'utf8')
  const setup = `
# #region DEBUG
packet_path = profile / "packet-trace.bin"
with packet_path.open("wb") as packet_file: packet_file.truncate(64+64*1048576)
packet_library = profile / "packet-trace.so"
subprocess.run(["cc","-shared","-fPIC","-O2","-x","c","-","-o",str(packet_library),"-ldl"],input=${JSON.stringify(library)},text=True,check=True,capture_output=True)
probe_env["LD_PRELOAD"] = str(packet_library)
probe_env["WS_STALL_FILE"] = str(packet_path)
probe_env["WS_STALL_RECEIVE_BUFFER"] = "${Number(process.env.WS_STALL_RECEIVE_BUFFER||0)}"
probe_env["WS_STALL_PAUSE_AT"] = "${Number(process.env.WS_STALL_PAUSE_AT||0)}"
probe_env["WS_STALL_PAUSE_MS"] = "${Number(process.env.WS_STALL_PAUSE_MS||0)}"
# #endregion DEBUG
`
  python = replace(python,'proc = subprocess.Popen(args, stdout=log, stderr=log, start_new_session=True, env=probe_env)',setup+'\nproc = subprocess.Popen(args, stdout=log, stderr=log, start_new_session=True, env=probe_env)')
  python = replace(python,'    log.close()',`    log.close()
    # #region DEBUG
    import base64, struct
    print(json.dumps({"event":"chrome-log","text":(profile/"chrome.log").read_text(errors="replace")[-131072:]}),flush=True)
    with packet_path.open("rb") as packet_file:
        header = packet_file.read(64)
        count = struct.unpack_from("<Q", header)[0]
        remaining = 64 + min(count,1048576)*64
        packet_file.seek(0)
        while remaining:
            data = packet_file.read(min(49152,remaining))
            if not data: raise RuntimeError("truncated packet trace")
            remaining -= len(data)
            print(json.dumps({"event":"packet-trace","data":base64.b64encode(data).decode()}),flush=True)
    # #endregion DEBUG`)
 }
 const pythonPath = join(directory,'lan-browser.py')
 await writeFile(pythonPath,python)
 let browser = frozen('f6d058cd','slowdown-20261022/linux-browser.mjs')
 browser = replace(browser,"new URL('./lan-browser.py', import.meta.url)",'new URL('+JSON.stringify(pathToFileURL(pythonPath).href)+')')
 browser = "import { appendFileSync } from 'node:fs'\n"+browser
 const tracePath = join(output,'receiver.bin')
 evidence.packetTrace = packets ? tracePath : undefined
 evidence.packetExperiment = {enabled:packets,receiveBuffer:Number(process.env.WS_STALL_RECEIVE_BUFFER||0),pauseAt:Number(process.env.WS_STALL_PAUSE_AT||0),pauseMilliseconds:Number(process.env.WS_STALL_PAUSE_MS||0),dropAt:Number(process.env.WS_STALL_DROP_AT||0),binary:process.env.WS_RESEARCH_BINARY_DIRECTORY}
 browser = replace(browser,"if (item.event === 'netlog') evidence.netlog = item","if (item.event === 'netlog') evidence.netlog = item\n    if (item.event === 'packet-trace') appendFileSync("+JSON.stringify(tracePath)+",Buffer.from(item.data,'base64'))\n    if (item.event === 'chrome-log') appendFileSync("+JSON.stringify(join(output,'chrome.log'))+",item.text)")
 const browserPath = join(directory,'browser.mjs')
 await writeFile(browserPath,browser)
 let sampling = frozen('eb1300d7','readiness-20261022/linux-browser.mjs')
 sampling = replace(sampling,"from '../slowdown-20261022/linux-browser.mjs'",'from '+JSON.stringify(pathToFileURL(browserPath).href))
 sampling = replace(sampling,"new URL('./udp-samples.py', import.meta.url)",'new URL('+JSON.stringify(new URL('../readiness-20261022/udp-samples.py',import.meta.url).href)+')')
 const samplingPath = join(directory,'sampling.mjs')
 await writeFile(samplingPath,sampling)
 return (await import(pathToFileURL(samplingPath).href)).startLinuxBrowser(ports,evidence)
}
// #endregion DEBUG
