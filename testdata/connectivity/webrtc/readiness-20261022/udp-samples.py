import json, pathlib, select, sys, time

INTERVAL_SECONDS = 0.02
MAXIMUM_SECONDS = 120
print(json.dumps({'event': 'ready', 'time': time.time_ns() // 1_000_000}), flush=True)
started = time.monotonic()
while time.monotonic() - started < MAXIMUM_SECONDS:
    sockets = []
    for family in ('udp', 'udp6'):
        for line in pathlib.Path('/proc/net/' + family).read_text().splitlines()[1:]:
            fields = line.split()
            port = int(fields[1].split(':')[1], 16)
            outgoing, incoming = (int(value, 16) for value in fields[4].split(':'))
            sockets.append({'port': port, 'family': family, 'rxBytes': incoming, 'txBytes': outgoing,
                            'drops': int(fields[12]), 'inode': fields[9]})
    print(json.dumps({'event': 'sample', 'time': time.time_ns() // 1_000_000, 'sockets': sockets}), flush=True)
    if select.select([sys.stdin], [], [], INTERVAL_SECONDS)[0]:
        break
print(json.dumps({'event': 'complete'}), flush=True)
