#!/usr/bin/env python3
"""Transparent, device-scoped TCP recorder. Output contains private raw XML.

Records both byte streams and receive boundaries without parsing or changing
them. EOF in one direction is forwarded as a half-close while the other keeps
draining. Encrypted TLS bytes, if negotiated, remain encrypted in recordings.
"""
import argparse
import json
import os
import select
import signal
import socket
import threading
import time

import private_output

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--listen', required=True)
parser.add_argument('--upstream', required=True)
parser.add_argument('--device-ip', required=True)
parser.add_argument('--output', required=True)
args = parser.parse_args()
os.umask(0o077)
directory = private_output.directory(args.output)
running = True


def address(value):
    host, port = value.rsplit(':', 1)
    return host, int(port)


def record(device, peer):
    session = directory / str(time.time_ns())
    try:
        session = private_output.directory(session)
        with device, socket.create_connection(address(args.upstream), timeout=10) as server, \
                private_output.create(session / 'device-to-server.xmpp') as outbound, \
                private_output.create(session / 'server-to-device.xmpp') as inbound, \
                private_output.create(session / 'events.jsonl') as events:
            device.settimeout(15)
            server.settimeout(15)
            with private_output.create(session / 'metadata.json') as metadata:
                metadata.write((json.dumps({
                    'peer': peer, 'upstream': server.getpeername(),
                    'kind': 'transparent TCP recording',
                    'time_basis': 'UTC Unix nanoseconds',
                }, indent=2) + '\n').encode())
            streams = {device: (server, outbound, 'device-to-server'),
                       server: (device, inbound, 'server-to-device')}
            offsets = {'device-to-server': 0, 'server-to-device': 0}
            while streams and running:
                readable, _, _ = select.select(list(streams), [], [], 1)
                for source in readable:
                    destination, output, direction = streams[source]
                    data = source.recv(65535)
                    if not data:
                        destination.shutdown(socket.SHUT_WR)
                        del streams[source]
                        continue
                    output.write(data)
                    events.write((json.dumps({'time_ns': time.time_ns(),
                                              'direction': direction,
                                              'offset': offsets[direction],
                                              'length': len(data)}) + '\n').encode())
                    offsets[direction] += len(data)
                    destination.sendall(data)
    except OSError as error:
        print(f'{session.name}: {error}', flush=True)
    finally:
        device.close()


def stop(*_):
    # Sessions notice within one select timeout and close their own sockets.
    global running
    running = False


signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
threads = []
with socket.socket() as listener:
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind(address(args.listen))
    listener.listen(4)
    listener.settimeout(1)
    print(f'Raw recorder listening on {args.listen}', flush=True)
    while running:
        try:
            device, peer = listener.accept()
        except socket.timeout:
            continue
        if peer[0] != args.device_ip:
            device.close()
            continue
        thread = threading.Thread(target=record, args=(device, peer))
        thread.start()
        threads.append(thread)
for thread in threads:
    thread.join()
