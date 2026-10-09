#!/usr/bin/env python3
"""Relay device TCP to Bosch; record private directional byte streams.

The device never negotiates STARTTLS, so recordings hold the whole plaintext
session, including SASL proofs. With --status-get, one fixed
GET /ecus/rrc/uiStatus built here is injected toward the device per
connection; it is never sent upstream and is not part of stock traffic, but
the device's reply is relayed to Bosch like any other stanza.
"""
import argparse
import os
import re
import select
import socket
import threading
import time
from xml.parsers import expat

import private_output

args = directory = None


class StanzaBoundary:
    """Tracks one direction's element depth, so an injected stanza never lands
    inside one being relayed."""

    max_declaration_bytes = 1024

    def __init__(self):
        self.depth, self.ok, self.closed, self.fed = 0, True, False, 0
        self.pending = b''
        self.parser = expat.ParserCreate()
        # expat 2.6+ defers a token cut by a read until the buffer doubles,
        # so a short completing read would leave the depth stale.
        self.parser.SetReparseDeferralEnabled(False)
        # A stream restart opens a fresh stream rather than nesting one.
        self.parser.StartElementHandler = lambda name, _: setattr(self, 'depth', 1 if name == 'stream:stream' else self.depth + 1)
        self.parser.EndElementHandler = lambda _: setattr(self, 'depth', self.depth - 1)

    def feed(self, data):
        if not self.ok:
            return
        try:
            data = self.pending + data
            self.pending = b''
            marker = b'<?xml'
            offset = 0
            while offset < len(data):
                start = data.find(marker, offset)
                if start < 0:
                    # TCP may split even the declaration's opening marker.
                    tail = data[offset:]
                    keep = next((n for n in range(len(marker) - 1, 0, -1)
                                 if tail.endswith(marker[:n])), 0)
                    self.pending = tail[len(tail) - keep:] if keep else b''
                    self._parse(tail[:len(tail) - keep])
                    break
                self._parse(data[offset:start])
                # Declaration-like text in CDATA, comments or attributes must
                # remain part of the parser's original token.
                boundary = self.depth <= 1 and self.parser.CurrentByteIndex == self.fed
                end_marker = start + len(marker)
                if boundary and end_marker == len(data):
                    self.pending = data[start:]
                    break
                if not boundary or data[end_marker:end_marker + 1] not in (b' ', b'\t', b'\r', b'\n'):
                    self._parse(marker)
                    offset = end_marker
                    continue
                # Stream restarts may repeat declarations. Strip only from the
                # probe parser, retaining fragments until their terminator.
                end = data.find(b'?>', end_marker)
                length = len(data) - start if end < 0 else end + 2 - start
                if length > self.max_declaration_bytes:
                    self.ok = False
                    break
                if end < 0:
                    self.pending = data[start:]
                    break
                offset = end + 2
            # expat stops at the last complete token: anything after it is a
            # tag cut by the read, even one ending in a literal '>'.
            self.closed = not self.pending and self.parser.CurrentByteIndex == self.fed
        except expat.ExpatError:
            self.ok = False  # unknown position: never inject

    def _parse(self, data):
        self.parser.Parse(data, False)
        self.fed += len(data)

    def between(self):
        return self.ok and self.closed and self.depth == 1


def address(value):
    host, port = value.rsplit(':', 1)
    return host, int(port)


def status_get(history):
    """Address the request using the device's own stream header."""
    header = re.search(rb'<stream:stream\b[^>]*>', history)
    attrs = dict(re.findall(rb'''\b(from|to)=['"]([A-Za-z0-9_.@-]+)['"]''', header[0])) if header else {}
    gateway, domain = attrs.get(b'from', b'').decode(), attrs.get(b'to', b'').decode()
    gateway = gateway.split('@')[0]
    serial = gateway.removeprefix('rrcgateway_')
    if not serial.isdigit() or not domain:
        return None
    return (f'<message to="{gateway}@{domain}" type="chat" from="rrccontact_{serial}@{domain}/localprobe">'
            '<body>GET /ecus/rrc/uiStatus HTTP/1.1\r\nUser-Agent: NefitEasy\r\n\r\n</body></message>').encode()


def save_response(history, label):
    for stanza in re.findall(rb'<message\b[^>]*>.*?</message>', history, re.S):
        if b'/localprobe' in stanza.split(b'>', 1)[0]:
            try:
                with private_output.create(directory / f'{label}-local-status-response.xml') as response:
                    response.write(stanza)
            except FileExistsError:
                return False
            return True
    return False


def relay(device, peer):
    label = str(time.time_ns())
    print(f'{label}: accepted {peer}', flush=True)
    try:
        with device, socket.create_connection(address(args.upstream), timeout=10) as upstream, \
                private_output.create(directory / f'{label}-device.bin') as outbound, \
                private_output.create(directory / f'{label}-cloud.bin') as inbound:
            device.settimeout(15)
            upstream.settimeout(15)
            peers = {device: (upstream, outbound), upstream: (device, inbound)}
            history = b''
            probe_sent = saved = not args.status_get
            # Only injection needs it, and it needs a newer expat than relaying.
            toward_device = StanzaBoundary() if args.status_get else None
            while peers:
                # Wait for startup HTTP traffic so the device session is ready.
                if not probe_sent and toward_device.between() and b'HTTP/1.0' in history and (request := status_get(history)):
                    device.sendall(request)
                    probe_sent = True
                    print(f'{label}: sent local status GET directly to device', flush=True)
                readable, _, _ = select.select(list(peers), [], [], 1)
                for source in readable:
                    destination, recording = peers[source]
                    data = source.recv(65536)
                    if not data:
                        # Forward EOF but keep draining the other direction.
                        destination.shutdown(socket.SHUT_WR)
                        del peers[source]
                        continue
                    recording.write(data)
                    if source is upstream and toward_device:
                        toward_device.feed(data)
                    if source is device and not saved:
                        history = (history + data)[-131072:]
                        if probe_sent and save_response(history, label):
                            saved = True
                            print(f'{label}: received local status response', flush=True)
                    destination.sendall(data)
    except OSError as error:
        print(f'{label}: {error}', flush=True)
    finally:
        print(f'{label}: closed', flush=True)


def main():
    global args, directory
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--listen', required=True, help='host:port the redirected device connects to')
    parser.add_argument('--device-ip', required=True, help='relay only this peer; others are dropped')
    parser.add_argument('--upstream', default='wa2-mz36-qrmzh6.bosch.de:5222')
    parser.add_argument('--output', default=private_output.default())
    parser.add_argument('--status-get', action='store_true', help='inject the local status GET once per connection')
    args = parser.parse_args()
    # Checked here, or every relayed connection would fail instead.
    if args.status_get and not hasattr(expat.XMLParserType, 'SetReparseDeferralEnabled'):
        parser.error('--status-get needs expat reparse-deferral control (Python 3.13, or 3.9.19/3.10.14/3.11.9/3.12.3 and later)')
    os.umask(0o077)
    directory = private_output.directory(args.output)
    with socket.socket() as listener:
        listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        listener.bind(address(args.listen))
        listener.listen(4)
        print(f'Listening on {args.listen} for {args.device_ip}; upstream {args.upstream}', flush=True)
        while True:
            device, peer = listener.accept()
            if peer[0] != args.device_ip:
                device.close()
                continue
            threading.Thread(target=relay, args=(device, peer), daemon=True).start()


if __name__ == '__main__':
    main()
