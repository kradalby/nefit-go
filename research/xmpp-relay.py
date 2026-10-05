#!/usr/bin/env python3
"""Relay device TCP to Bosch unchanged; record private directional byte streams.

This observes the initial XMPP negotiation, not plaintext after STARTTLS.
The upstream is the library default until device traffic identifies its server.
"""
import os
import re
import select
import socket
import threading
import time
from pathlib import Path

os.umask(0o077)
directory = Path('/tmp/nefit-research')
directory.mkdir(exist_ok=True)


def relay(device, peer):
    label = f'{time.time_ns()}'
    print(f'{label}: accepted {peer}', flush=True)
    try:
        with device, socket.create_connection(
                ('wa2-mz36-qrmzh6.bosch.de', 5222), timeout=10) as upstream:
            device.settimeout(15)
            upstream.settimeout(15)
            with (directory / f'{label}-device.bin').open('xb') as outbound, \
                    (directory / f'{label}-cloud.bin').open('xb') as inbound:
                destinations = {device: (upstream, outbound), upstream: (device, inbound)}
                device_history = b''
                probe_sent = False
                while True:
                    # The private request file permits exactly one local GET.
                    # It is never sent upstream to Bosch.
                    probe = directory / 'local-status-request.xml'
                    if not probe_sent and b'HTTP/1.0' in device_history and probe.exists():
                        request = probe.read_bytes()
                        if b'GET /ecus/rrc/uiStatus HTTP/1.1' not in request or b'PUT ' in request:
                            raise ValueError('only the fixed status GET is permitted')
                        device.sendall(request)
                        probe_sent = True
                        print(f'{label}: sent local status GET directly to device', flush=True)
                    readable, _, _ = select.select([device, upstream], [], [], 1)
                    if not readable:
                        continue
                    for source in readable:
                        data = source.recv(65536)
                        if not data:
                            return
                        destination, recording = destinations[source]
                        recording.write(data)
                        recording.flush()
                        if source is device:
                            device_history = (device_history + data)[-131072:]
                            if probe_sent:
                                for stanza in re.findall(rb'<message\b[^>]*>.*?</message>', device_history, re.S):
                                    header = stanza.split(b'>', 1)[0]
                                    if b'/localprobe' in header:
                                        response = directory / 'local-status-response.xml'
                                        if not response.exists():
                                            response.write_bytes(stanza)
                                            print(f'{label}: received local status response', flush=True)
                        destination.sendall(data)
    except OSError as error:
        print(f'{label}: {error}', flush=True)
    finally:
        device.close()
        print(f'{label}: closed', flush=True)


with socket.socket() as listener:
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind(('10.65.0.27', 5222))
    listener.listen(4)
    print('Listening on 10.65.0.27:5222; upstream is library default Bosch host', flush=True)
    while True:
        device, peer = listener.accept()
        threading.Thread(target=relay, args=(device, peer), daemon=True).start()
