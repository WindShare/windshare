"""Replay deterministic, varied controller histories in Go and TypeScript."""
from pathlib import Path
import hashlib
import json
import os
import subprocess
import sys

ROOT=Path(__file__).resolve().parents[4]
HERE=Path(__file__).resolve().parent
OUTPUT=ROOT/'.tmp/sampling-stress-tools'
subprocess.run([sys.executable,str(HERE/'generate.py')],cwd=ROOT,check=True)
executable=OUTPUT/('conformance.exe' if os.name=='nt' else 'conformance')
subprocess.run(['go','build','-o',str(executable),str(HERE/'conformance.go')],cwd=ROOT,check=True)
with (OUTPUT/'generated.json').open('rb') as inputs, (OUTPUT/'native.json').open('wb') as results:
    subprocess.run([str(executable)],cwd=ROOT,stdin=inputs,stdout=results,check=True)
result=subprocess.run(['node','--import','./testdata/connectivity/webrtc/slowdown-20261022/loader.mjs',str(HERE/'conformance.mjs')],cwd=ROOT,text=True,stdout=subprocess.PIPE,check=True)
report=json.loads(result.stdout)
report['seed']=20261024
report['inputSHA256']=hashlib.sha256((OUTPUT/'generated.json').read_bytes()).hexdigest()
report['nativeDecisionsSHA256']=hashlib.sha256((OUTPUT/'native.json').read_bytes()).hexdigest()
(OUTPUT/'result.json').write_text(json.dumps(report,indent=2)+'\n')
print(json.dumps(report))
