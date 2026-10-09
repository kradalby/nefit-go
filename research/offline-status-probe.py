#!/usr/bin/env python3
"""One-session local XMPP experiment; no connection to Bosch is made.

Accepts the device's DIGEST-MD5 response without verifying its unknown password.
Tests whether the device requires server rspauth. Sends only a status GET.
Run only behind the device-specific firewall/NAT rule; this is not a server
implementation suitable for general use. Exits after one reply or a timeout.
The capture holds the device's SASL exchange. --mechanism PLAIN records the
device's XMPP credentials in clear text if the device accepts PLAIN.
"""
import argparse
import base64
import os
import re
import secrets
import socket
import time
import xml.etree.ElementTree as ET
from xml.sax.saxutils import quoteattr

import private_output

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--listen', required=True, help='host:port the redirected device connects to')
parser.add_argument('--device-ip', required=True, help='accept only this peer')
parser.add_argument('--output', default=private_output.default())
parser.add_argument('--mechanism', choices=['DIGEST-MD5', 'PLAIN'], default='DIGEST-MD5')
parser.add_argument('--dummy-proof', action='store_true', help='Send a formatted but invalid rspauth proof')
args = parser.parse_args()
os.umask(0o077)
domain = 'wa2-mz36-qrmzh6.bosch.de'
directory = private_output.directory(args.output)
run = directory / f'offline-{time.time_ns()}'
pattern = re.compile(
    rb"(?:<\?xml[^>]*\?>)|(?:<stream:stream\b[^>]*>)|"
    rb"(?:<(auth|response|iq|presence|message)\b[^>]*(?:/>|>.*?</\1>))", re.S)


def stream():
    return (f'<stream:stream xmlns:stream="http://etherx.jabber.org/streams" '
            f'xmlns="jabber:client" from="{domain}" id="local-research" version="1.0">')


with socket.socket() as listener:
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    host, port = args.listen.rsplit(':', 1)
    listener.bind((host, int(port)))
    listener.listen(1)
    listener.settimeout(90)
    print('Offline probe listening; no cloud socket', flush=True)
    device, peer = listener.accept()
    if peer[0] != args.device_ip:
        device.close()
        raise RuntimeError('unexpected peer')
    print('Device connected', flush=True)
    with device, private_output.create(f'{run}-device.bin') as capture:
        device.settimeout(20)
        pending = b''
        authenticated = False
        jid = ''
        requested = False
        end = time.monotonic() + 60

        def send(text):
            device.sendall(text.encode())

        while time.monotonic() < end:
            data = device.recv(65535)
            if not data:
                raise RuntimeError('device closed connection')
            capture.write(data)
            pending += data
            while True:
                match = pattern.search(pending)
                if not match:
                    break
                stanza = match[0]
                pending = pending[match.end():]
                if stanza.startswith(b'<?xml'):
                    continue
                if stanza.startswith(b'<stream:stream'):
                    if not jid:
                        name = re.search(rb"\bfrom=['\"]([^'\"]+)", stanza)[1].decode()
                        jid = name + '@' + domain if '@' not in name else name
                    send(stream())
                    if authenticated:
                        send('<stream:features><bind xmlns="urn:ietf:params:xml:ns:xmpp-bind"/>'
                             '<session xmlns="urn:ietf:params:xml:ns:xmpp-session"/></stream:features>')
                    else:
                        send('<stream:features><mechanisms xmlns="urn:ietf:params:xml:ns:xmpp-sasl">'
                             f'<mechanism>{args.mechanism}</mechanism></mechanisms></stream:features>')
                    continue
                element = ET.fromstring(stanza)
                kind = element.tag.rsplit('}', 1)[-1]
                print('Received', kind, flush=True)
                if kind == 'auth':
                    if args.mechanism == 'PLAIN':
                        if element.attrib.get('mechanism') != 'PLAIN':
                            raise RuntimeError('device selected an unadvertised mechanism')
                        credential = base64.b64decode(element.text or '', validate=True)
                        fields = credential.split(b'\x00')
                        if len(fields) != 3:
                            raise RuntimeError('malformed PLAIN authentication')
                        with private_output.create(f'{run}-plain-credentials.bin') as saved:
                            saved.write(credential)
                        print('Received PLAIN authentication; credentials saved privately', flush=True)
                        authenticated = True
                        send('<success xmlns="urn:ietf:params:xml:ns:xmpp-sasl"/>')
                        continue
                    challenge = (f'realm="{domain}",nonce="{secrets.token_hex(16)}",'
                                 'charset=utf-8,algorithm=md5-sess')
                    send('<challenge xmlns="urn:ietf:params:xml:ns:xmpp-sasl">' +
                         base64.b64encode(challenge.encode()).decode() + '</challenge>')
                elif kind == 'response':
                    authenticated = True
                    if args.dummy_proof:
                        proof = base64.b64encode(b'rspauth=' + b'0' * 32).decode()
                        send('<success xmlns="urn:ietf:params:xml:ns:xmpp-sasl">' + proof + '</success>')
                    else:
                        send('<success xmlns="urn:ietf:params:xml:ns:xmpp-sasl"/>')
                elif kind == 'iq':
                    identity = quoteattr(element.attrib['id'])
                    bind = element.find('{urn:ietf:params:xml:ns:xmpp-bind}bind')
                    if bind is not None:
                        resource = bind.findtext('{urn:ietf:params:xml:ns:xmpp-bind}resource', 'RRC-RestApi')
                        send(f'<iq type="result" id={identity}><bind xmlns="urn:ietf:params:xml:ns:xmpp-bind">'
                             f'<jid>{jid}/{resource}</jid></bind></iq>')
                    else:
                        send(f'<iq type="result" id={identity} from="{domain}" to="{jid}/RRC-RestApi"/>')
                elif kind == 'presence' and not requested:
                    serial = jid.split('@')[0].removeprefix('rrcgateway_')
                    send(f'<message to="{jid}" type="chat" from="rrccontact_{serial}@{domain}/localprobe">'
                         '<body>GET /ecus/rrc/uiStatus HTTP/1.1\r\nUser-Agent: NefitEasy\r\n\r\n</body></message>')
                    requested = True
                    print('Sent offline status GET', flush=True)
                elif kind == 'message' and '/localprobe' in element.attrib.get('to', ''):
                    with private_output.create(f'{run}-status-response.xml') as response:
                        response.write(stanza)
                    body = next((e.text or '' for e in element if e.tag.rsplit('}', 1)[-1] == 'body'), '')
                    print('Offline response:', body.splitlines()[0] if body else '(empty)', flush=True)
                    raise SystemExit(0)
        raise RuntimeError('offline experiment timed out')
