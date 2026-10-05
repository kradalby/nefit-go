#!/usr/bin/env python3
"""Build structurally faithful, sanitized stanza fixtures from private recordings.

Encrypted bodies are opaque replacements, NOT decrypted application fixtures.
SASL proofs are invalid synthetic values, NOT authentication replay credentials.
"""
import argparse
import base64
import hashlib
import json
from pathlib import Path
import re
import xml.etree.ElementTree as ET

SASL_NS = 'urn:ietf:params:xml:ns:xmpp-sasl'


def sanitize_sasl(s, exchange):
    element = ET.fromstring(s)
    for child in element.iter():
        if child is not element and child.tag in {f'{{{SASL_NS}}}{kind}' for kind in ('auth', 'challenge', 'response', 'success')}:
            raise ValueError('Nested SASL payload is unsupported')
    namespace, _, kind = element.tag.rpartition('}')
    namespace = namespace.lstrip('{')
    if kind not in ('auth', 'challenge', 'response', 'success'):
        return s
    if namespace != SASL_NS or list(element):
        raise ValueError('Unsupported SASL representation')
    mechanism = exchange.get('mechanism')
    if kind == 'auth':
        mechanism = element.attrib.get('mechanism')
        if mechanism not in ('PLAIN', 'DIGEST-MD5'):
            raise ValueError('Unsupported SASL mechanism')
        exchange['mechanism'] = mechanism
    payload = re.sub(r'[ \t\r\n]', '', element.text or '')
    if not payload or payload == '=':
        return s
    try:
        decoded = base64.b64decode(payload, validate=True)
    except ValueError as error:
        raise ValueError('Invalid SASL base64') from error
    if mechanism == 'PLAIN' or b'\x00' in decoded:
        if kind not in ('auth', 'response') or len(decoded.split(b'\x00')) != 3:
            raise ValueError('Invalid SASL PLAIN payload')
        authorization, _, _ = decoded.split(b'\x00')
        replacement = (b'fixture-authorization' if authorization else b'') + b'\x00fixture-user\x00fixture-password'
    else:
        if mechanism not in (None, 'DIGEST-MD5') or kind == 'auth':
            raise ValueError('Unsupported SASL payload')
        # Parse the complete directive list; partially understood payloads must
        # never be copied to public fixtures as if they had been redacted.
        text = decoded.decode('utf-8')
        directives = []
        offset = 0
        directive = re.compile(r'\s*([a-z-]+)\s*=\s*("(?:[^"\\]|\\.)*"|[^,\s]+)\s*(?:,|$)')
        allowed = {'realm', 'nonce', 'qop', 'charset', 'algorithm', 'username', 'cnonce', 'nc', 'digest-uri', 'response', 'rspauth', 'authzid', 'maxbuf', 'cipher'}
        synthetic = {'realm': 'fixture-realm', 'username': 'rrcgateway_123456789', 'authzid': 'fixture-authorization', 'nonce': 'fixture-nonce', 'cnonce': 'fixture-cnonce', 'response': '0'*32, 'rspauth': '0'*32, 'digest-uri': 'xmpp/fixture.example'}
        while offset < len(text):
            match = directive.match(text, offset)
            if not match or match[1] not in allowed:
                raise ValueError('Unsupported SASL DIGEST directive')
            key, value = match[1], match[2]
            if key in synthetic:
                value = '"' + synthetic[key] + '"' if value.startswith('"') else synthetic[key]
            directives.append(key + '=' + value)
            offset = match.end()
        if not directives:
            raise ValueError('Empty SASL DIGEST payload')
        replacement = ','.join(directives).encode()
    encoded = base64.b64encode(replacement).decode()
    # Keep lexical CDATA and surrounding whitespace where captures used them.
    match = re.fullmatch(r'(<[^>]+>)(.*)(</[^>]+>)', s, re.S)
    if not match:
        raise ValueError('Unsupported SASL element')
    content = match[2]
    cdata = re.fullmatch(r'(\s*)<!\[CDATA\[(.*)\]\]>(\s*)', content, re.S)
    if cdata:
        inner = cdata[2]
        leading = inner[:len(inner)-len(inner.lstrip())]
        trailing = inner[len(inner.rstrip()):]
        content = cdata[1] + '<![CDATA[' + leading + encoded + trailing + ']]>' + cdata[3]
    else:
        leading = content[:len(content)-len(content.lstrip())]
        trailing = content[len(content.rstrip()):]
        content = leading + encoded + trailing
    return match[1] + content + match[3]


pattern = re.compile(rb'<(stream:features|auth|challenge|response|success|failure|iq|presence|message)\b[^>]*(?:/>|(?<!/)>.*?</\1\s*>)', re.S)

def sanitize(raw, exchange=None):
    s = raw.decode('utf-8')
    s = re.sub(r'rrc(gateway|contact)_\d+', r'rrc\1_123456789', s)
    # Preserve JID roles and resource shape without recording personal identifiers.
    s = re.sub(r'(xmpp-adapter_)\d+', r'\g<1>1000000000000', s)
    s = re.sub(r'([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})', '00000000-0000-4000-8000-000000000000', s, flags=re.I)
    def body(m):
        content = m[2]
        cdata = content.startswith('<![CDATA[') and content.endswith(']]>')
        if cdata: content = content[9:-3]
        # Ciphertext is always a final base64 blob, either alone or after HTTP headers.
        match = re.search(r'[A-Za-z0-9+/]{32,}={0,2}$', content)
        if match:
            cipher = base64.b64decode(match[0])
            replacement = base64.b64encode(bytes(len(cipher))).decode()
            content = content[:match.start()]+replacement
        if cdata: content = '<![CDATA[' + content + ']]>'
        return m[1]+content+m[3]
    s = re.sub(r'(<body\b[^>]*>)(.*?)(</body>)', body, s, flags=re.S)
    # Stanzas inherit these namespaces from stream headers on the original wire.
    name = re.match(r'<([^\s/>]+)', s)[1]
    if name == 'stream:features' and not re.search(r'^<[^>]*\bxmlns:stream=', s):
        s = s.replace('<stream:features', '<stream:features xmlns:stream="http://etherx.jabber.org/streams"', 1)
    elif name in ('message','iq','presence') and not re.search(r'^<[^>]*\bxmlns=', s):
        s = s.replace('<'+name, '<'+name+' xmlns="jabber:client"', 1)
    s = sanitize_sasl(s, exchange if exchange is not None else {})
    ET.fromstring(s)  # Fail closed on invalid redaction or incomplete XML.
    return s.encode(), name

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('captures', type=Path)
    p.add_argument('output', type=Path)
    a = p.parse_args()
    a.output.mkdir(parents=True, exist_ok=True)
    # Collect previous generated files, but do not delete until all inputs validate.
    previous = []
    for index_name in ('manifest.json', 'streams.json'):
        index = a.output / index_name
        if index.exists():
            for entry in json.loads(index.read_text()):
                filename = entry['file']
                if not re.fullmatch(r'(?:[0-9]{4}-[a-z-]+\.xml|stream-[0-9]+\.open)', filename):
                    raise ValueError('Unsafe fixture filename in previous manifest')
                previous.append(filename)
    outputs = {}
    manifest = []
    streams = []
    stream_seen = set()
    seen = {}

    sources = sorted(a.captures.glob('*.bin')) + sorted(a.captures.glob('*.xml')) + sorted(a.captures.rglob('*.xmpp'))
    for source in sources:
        raw = source.read_bytes()
        exchange = {}
        for opening in re.finditer(rb'<stream:stream\b[^>]*>', raw):
            header = opening[0].decode()
            header = re.sub(r'rrc(gateway|contact)_\d+', r'rrc\1_123456789', header)
            header = re.sub(r"\bid=([\"'])[^\"']*\1", 'id="fixture-stream"', header)
            if header not in stream_seen:
                stream_seen.add(header)
                filename = f'stream-{len(streams):02d}.open'
                outputs[filename] = header.encode()
                streams.append({'file': filename, 'recording': source.relative_to(a.captures).as_posix(), 'offset': opening.start()})
        for match in pattern.finditer(raw):
            cleaned, kind = sanitize(match[0], exchange)
            digest = hashlib.sha256(cleaned).hexdigest()
            provenance = {'recording': source.relative_to(a.captures).as_posix(), 'offset': match.start(), 'bytes': len(match[0])}
            if digest in seen:
                seen[digest]['occurrences'].append(provenance)
                continue
            filename = f'{len(manifest):04d}-{kind.replace(":", "-")}.xml'
            outputs[filename] = cleaned
            element = ET.fromstring(cleaned)
            body_node = element.find('{jabber:client}body')
            expected = {'namespace': element.tag.split('}')[0].lstrip('{')}
            if body_node is not None:
                text = body_node.text or ''
                expected['body_sha256'] = hashlib.sha256(text.encode()).hexdigest()
                if text.startswith('HTTP/'):
                    expected['status_code'] = int(text.split(' ', 2)[1])
            entry = {'file': filename, 'kind': kind, 'sha256': digest, 'expected': expected, 'occurrences': [provenance]}
            manifest.append(entry)
            seen[digest] = entry
    outputs['manifest.json'] = (json.dumps(manifest, indent=2)+'\n').encode()
    outputs['streams.json'] = (json.dumps(streams, indent=2)+'\n').encode()
    for filename, data in outputs.items():
        (a.output / filename).write_bytes(data)
    for filename in previous:
        if filename not in outputs:
            (a.output / filename).unlink(missing_ok=True)
    print(f'{len(manifest)} distinct sanitized stanzas from {len(sources)} recordings')


if __name__ == '__main__':
    main()
