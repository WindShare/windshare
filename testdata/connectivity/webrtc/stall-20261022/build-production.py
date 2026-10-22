import hashlib
import importlib.util
import json
import shutil
import stat
import subprocess

from build_support import ROOT, HERE, BUILD, module_file, prepare_lab, upstream

relay = prepare_lab()
original = upstream('github.com/pion/sctp@v1.11.1')
metadata = {}
for arm in ['before', 'after']:
    target = BUILD / ('production-'+arm)
    target.mkdir(parents=True, exist_ok=True)
    module = original
    if arm == 'after':
        module = target / 'sctp'
        if not module.exists():
            shutil.copytree(original, module)
        source = module / 'association.go'
        source.chmod(stat.S_IREAD | stat.S_IWRITE)
        spec = importlib.util.spec_from_file_location('rack_repair', HERE / 'rack-repair.py')
        repair = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(repair)
        source.write_text(repair.apply((original / 'association.go').read_text()))
        subprocess.run(['gofmt', '-w', str(source)], check=True)
    # Both arms explicitly choose SCTP: this remains a valid comparison after the
    # production checkout adopts the repair.
    mod = module_file(target, {'github.com/pion/sctp': module})
    subprocess.run(['go', 'build', '-modfile='+str(mod), '-o',
                    str(target / 'wind.exe'), './cmd/wind'], cwd=ROOT, check=True)
    shutil.copyfile(relay, target / 'relay-native.exe')
    metadata[arm] = {
        'senderSHA256': hashlib.sha256((target / 'wind.exe').read_bytes()).hexdigest(),
        'associationSHA256': hashlib.sha256((module / 'association.go').read_bytes()).hexdigest(),
    }
metadata['sourceRevision'] = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT).decode().strip()
metadata['repairSHA256'] = hashlib.sha256((HERE / 'rack-repair.py').read_bytes()).hexdigest()
(BUILD / 'production-builds.json').write_text(json.dumps(metadata, indent=2)+'\n')
