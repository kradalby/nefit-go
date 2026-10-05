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
pattern = re.compile(rb'<(stream:features|auth|challenge|response|success|failure|iq|presence|message)\b[^>]*(?:/>|(?<!/)>.*?</\1\s*>)', re.S)

def sanitize(raw):
    s = raw.decode('utf-8')
    s = re.sub(r'rrc(gateway|contact)_\d+', r'rrc\1_123456789', s)
    # Preserve JID roles and resource shape without recording personal identifiers.
    s = re.sub(r'(xmpp-adapter_)\d+', r'\g<1>1000000000000', s)
    s = re.sub(r'([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})', '00000000-0000-4000-8000-000000000000', s, flags=re.I)
    def sasl(m):
        decoded = base64.b64decode(m[2]).decode('utf-8')
        decoded = re.sub(r'rrc(gateway|contact)_\d+', r'rrc\1_123456789', decoded)
        for field, replacement in [('nonce','fixture-nonce'), ('cnonce','fixture-cnonce'), ('response','0'*32), ('rspauth','0'*32)]:
            decoded = re.sub(r'\b'+field+r'=("[^"]*"|[^,]*)', field+'="'+replacement+'"', decoded)
        return m[1]+base64.b64encode(decoded.encode()).decode()+m[3]
    s = re.sub(r'(<(?:challenge|response|success)\b[^>]*>)([A-Za-z0-9+/=]+)(</[^>]+>)', sasl, s)
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
    ET.fromstring(s)  # Fail closed on invalid redaction or incomplete XML.
    return s.encode(), name

sources = sorted(a.captures.glob('*.bin')) + sorted(a.captures.glob('*.xml')) + sorted(a.captures.rglob('*.xmpp'))
for source in sources:
    raw = source.read_bytes()
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
        cleaned, kind = sanitize(match[0])
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
