#!/usr/bin/env python3
"""Transparent, device-scoped TCP recorder. Output contains private raw XML.

Records both byte streams and receive boundaries without parsing or changing
them. Encrypted TLS bytes, if negotiated, remain encrypted in these recordings.
"""
import argparse
import json
import os
from pathlib import Path
import select
import signal
import socket
import threading
import time

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--listen', required=True)
parser.add_argument('--upstream', required=True)
parser.add_argument('--device-ip', required=True)
parser.add_argument('--output', required=True)
args = parser.parse_args()
os.umask(0o077)
directory = Path(args.output)
directory.mkdir(parents=True, exist_ok=True)
connections = set()
lock = threading.Lock()
running = True


def address(value):
    host, port = value.rsplit(':', 1)
    return host, int(port)


def record(device, peer):
    session = directory / str(time.time_ns())
    session.mkdir()
    try:
        with device, socket.create_connection(address(args.upstream), timeout=10) as server:
            with lock:
                connections.add(server)
            device.settimeout(15)
            server.settimeout(15)
            (session / 'metadata.json').write_text(json.dumps({
                'peer': peer, 'upstream': server.getpeername(),
                'kind': 'transparent TCP recording',
                'time_basis': 'UTC Unix nanoseconds',
            }, indent=2) + '\n')
            with (session / 'device-to-server.xmpp').open('xb', buffering=0) as outbound, \
                    (session / 'server-to-device.xmpp').open('xb', buffering=0) as inbound, \
                    (session / 'events.jsonl').open('x', buffering=1) as events:
                streams = {device: (server, outbound, 'device-to-server'),
                           server: (device, inbound, 'server-to-device')}
                offsets = {'device-to-server': 0, 'server-to-device': 0}
                while running:
                    readable, _, _ = select.select([device, server], [], [], 1)
                    for source in readable:
                        data = source.recv(65535)
                        if not data:
                            return
                        destination, output, direction = streams[source]
                        output.write(data)
                        events.write(json.dumps({'time_ns': time.time_ns(),
                                                 'direction': direction,
                                                 'offset': offsets[direction],
                                                 'length': len(data)}) + '\n')
                        offsets[direction] += len(data)
                        destination.sendall(data)
    except OSError as error:
        print(f'{session.name}: {error}', flush=True)
    finally:
        with lock:
            connections.discard(device)
            if 'server' in locals():
                connections.discard(server)
        device.close()


listener = socket.socket()
listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
listener.bind(address(args.listen))
listener.listen(4)
listener.settimeout(1)


def stop(*_):
    global running
    running = False
    with lock:
        for connection in connections:
            try:
                connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            connection.close()


signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
print(f'Raw recorder listening on {args.listen}', flush=True)
with listener:
    while running:
        try:
            device, peer = listener.accept()
        except socket.timeout:
            continue
        if peer[0] != args.device_ip:
            device.close()
            continue
        with lock:
            connections.add(device)
        threading.Thread(target=record, args=(device, peer), daemon=True).start()
