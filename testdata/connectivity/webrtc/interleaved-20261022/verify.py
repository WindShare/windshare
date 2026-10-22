import hashlib, json, pathlib, subprocess

root = pathlib.Path(__file__).resolve().parents[4]
snapshots = root / '.tmp/webrtc-interleaved-20261022'
verified = []
for arm, revision in [('old','197fe6f4'),('new','86446a6a')]:
    commit = subprocess.check_output(['git','rev-parse',revision],cwd=root,text=True).strip()
    tree = subprocess.check_output(['git','ls-tree','-rz',commit,'--','web'],cwd=root).split(b'\0')
    count = 0
    for entry in tree:
        if not entry: continue
        metadata, raw_path = entry.split(b'\t',1)
        mode, kind, expected = metadata.split()
        assert kind == b'blob'
        path = raw_path.decode('utf-8')
        data = (snapshots/arm/path).read_bytes()
        actual = hashlib.sha1(b'blob '+str(len(data)).encode()+b'\0'+data).hexdigest()
        assert actual == expected.decode(), f'{arm}: snapshot changed: {path}'
        count += 1
    verified.append({'arm':arm,'revision':commit,'verifiedFiles':count})
(root/'testdata/connectivity/webrtc/interleaved-20261022/snapshots.json').write_text(json.dumps(verified,indent=2)+'\n',encoding='utf-8')
print(json.dumps(verified))
