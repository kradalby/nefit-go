#!/usr/bin/env python3
"""Build structurally faithful, sanitized stanza fixtures from private recordings.

Encrypted bodies are opaque replacements, NOT decrypted application fixtures.
SASL proofs are invalid synthetic values, NOT authentication replay credentials.
"""
import argparse
import base64
import hashlib
import html
import json
from pathlib import Path
import re
import sys
import unicodedata
import urllib.parse
import xml.etree.ElementTree as ET
import xml.parsers.expat as expat

SASL_NS = 'urn:ietf:params:xml:ns:xmpp-sasl'
STREAM_NS = 'http://etherx.jabber.org/streams'
OPEN_TAG = re.compile(rb'<[^\s/>]+(?:[^<>\"\']|\"[^\"]*\"|\'[^\']*\')*>')
ATTRIBUTE = re.compile(rb'([^\s=/>]+)\s*=\s*([\"\'])(.*?)\2', re.S)
IDENTITY = re.compile(r'rrc(?:gateway|contact)_\d+|xmpp-adapter_\d+|[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}', re.I)
SERIAL, ADAPTER, UUID = '123456789', '1000000000000', '00000000-0000-4000-8000-000000000000'
SYNTHETIC = re.compile(f'rrc(?:gateway|contact)_{SERIAL}|xmpp-adapter_{ADAPTER}|{UUID}', re.I)
SASL_PAYLOADS = {f'{SASL_NS}}}{kind}' for kind in ('auth', 'challenge', 'response', 'success')}
# Entity capabilities hash public service discovery data, not secrets.
CAPS_VER = ('http://jabber.org/protocol/caps}c', 'ver')
# Human-readable text can echo user names, hosts or failure details.
FREE_TEXT = {f'{SASL_NS}}}text', 'urn:ietf:params:xml:ns:xmpp-stanzas}text', 'jabber:iq:version}name', 'jabber:iq:version}os',
             # Legacy authentication and registration carry credentials in clear.
             'jabber:iq:auth}password', 'jabber:iq:auth}digest', 'jabber:iq:register}password'}
BLOB = re.compile(r'[A-Za-z0-9+/_-]{16,}=*')
# Paths, MIME types and service names split into camelCase words; base64 or
# hex keys, ciphertext and unhyphenated UUIDs practically never do.
WORD = re.compile(r'(?:[A-Z]{1,4}|[A-Za-z][a-z]+(?:[A-Z][a-z]+)*[A-Z]{0,2})[0-9]?')


def identity_replacement(value):
    if value.lower().startswith(('rrcgateway_', 'rrccontact_')):
        return value.split('_', 1)[0] + '_' + SERIAL
    if value.lower().startswith('xmpp-adapter_'):
        return value.split('_', 1)[0] + '_' + ADAPTER
    return UUID


def lexical_value(raw, offset, attribute=False, cdata=False):
    """Map decoded XML characters to their original UTF-8 byte spans."""
    values, positions = [], []
    token = r'\r\n|\r|[\s\S]' if cdata else r'&[^;]+;|\r\n|\r|[\s\S]'
    entities = {'&amp;': '&', '&lt;': '<', '&gt;': '>', '&quot;': '"', '&apos;': "'"}
    for match in re.finditer(token, raw.decode()):
        lexical = match[0]
        if not cdata and lexical.startswith('&'):
            if lexical.startswith('&#x'):
                value = chr(int(lexical[3:-1], 16))
            elif lexical.startswith('&#'):
                value = chr(int(lexical[2:-1]))
            else:
                value = entities[lexical]
        elif lexical in ('\r', '\r\n'):
            value = ' ' if attribute else '\n'
        elif attribute and lexical in ('\t', '\n'):
            value = ' '
        else:
            value = lexical
        size = len(lexical.encode())
        values.append(value)
        positions.append((offset, offset + size))
        offset += size
    return ''.join(values), positions


def sanitize_identities(s, stream=False):
    raw = s.encode()
    suffix = b''
    if stream:
        opening = OPEN_TAG.fullmatch(raw)
        if not opening or raw.endswith(b'/>'):
            raise ValueError('Unsupported stream header spelling')
        name = re.match(rb'<([^\s/>]+)', raw)[1]
        suffix = b'</' + name + b'>'
    parser = expat.ParserCreate(namespace_separator='}')
    parser.ordered_attributes = True
    edits = []
    roots = []

    def redact(value, positions, stream_id=False):
        matches = [(0, len(value), 'fixture-stream')] if stream_id else [
            (match.start(), match.end(), identity_replacement(match[0]))
            for match in IDENTITY.finditer(value)
        ]
        for begin, end, replacement in matches:
            if value[begin:end] == replacement:
                continue
            if begin == end:
                raise ValueError('Empty stream ID is unsupported')
            # A semantic value can cross CDATA/comment boundaries. Replace its
            # character spans without removing those lexical delimiters.
            spans = []
            for first, last in positions[begin:end]:
                if spans and spans[-1][1] == first:
                    spans[-1] = (spans[-1][0], last)
                else:
                    spans.append((first, last))
            for first, last in spans:
                edits.append((first, last, replacement.encode()))
                replacement = ''

    def unsupported(*args):
        raise ValueError('Unsupported XML declaration or processing instruction')

    def start(name, attrs):
        if any(identity_replacement(match[0]) != match[0] for match in IDENTITY.finditer(name)):
            raise ValueError('Identity in XML name or namespace is unsupported')
        audit_values([name])
        roots.append(name)
        opening = OPEN_TAG.match(raw, parser.CurrentByteIndex)
        if not opening:
            raise ValueError('Unsupported XML opening tag')
        values = iter(zip(attrs[::2], attrs[1::2]))
        for attr in ATTRIBUTE.finditer(opening[0]):
            lexical_name = attr[1].decode()
            audit_values([lexical_name])
            # Names, prefixes included, are kept verbatim, so none may hold one.
            if any(identity_replacement(match[0]) != match[0] for match in IDENTITY.finditer(lexical_name)):
                raise ValueError('Identity in XML name or namespace is unsupported')
            value, positions = lexical_value(attr[3], opening.start() + attr.start(3), attribute=True)
            if lexical_name == 'xmlns' or lexical_name.startswith('xmlns:'):
                if any(identity_replacement(match[0]) != match[0] for match in IDENTITY.finditer(value)):
                    raise ValueError('Identity in XML namespace is unsupported')
                continue
            expanded, parsed = next(values, (None, None))
            if parsed != value:
                raise ValueError('Unsupported XML attribute representation')
            redact(value, positions, stream and expanded == 'id')
        if next(values, None) is not None:
            raise ValueError('Unsupported XML attributes')

    parser.StartElementHandler = start
    parser.StartDoctypeDeclHandler = unsupported
    parser.EntityDeclHandler = unsupported
    parser.ExternalEntityRefHandler = unsupported
    parser.ProcessingInstructionHandler = unsupported
    parser.XmlDeclHandler = unsupported
    try:
        parser.Parse(raw + suffix, True)
    except expat.ExpatError as error:
        raise ValueError('Invalid XML identity representation') from error
    if stream and roots != [STREAM_NS + '}stream']:
        raise ValueError('Unsupported stream namespace')

    text, positions = [], []

    def flush():
        redact(''.join(text), positions)
        text.clear()
        positions.clear()

    token = rb'<!\[CDATA\[.*?\]\]>|<!--.*?-->|<(?:[^<>\"\']|\"[^\"]*\"|\'[^\']*\')*>|[^<]+'
    offset = 0
    for part in re.finditer(token, raw, re.S):
        if part.start() != offset:
            raise ValueError('Unsupported XML text representation')
        offset = part.end()
        if part[0].startswith(b'<!--'):
            value, spans = lexical_value(part[0][4:-3], part.start() + 4, cdata=True)
            redact(value, spans)
            continue
        if part[0].startswith(b'<![CDATA['):
            value, spans = lexical_value(part[0][9:-3], part.start() + 9, cdata=True)
        elif part[0].startswith(b'<'):
            flush()
            continue
        else:
            value, spans = lexical_value(part[0], part.start())
        text.append(value)
        positions.extend(spans)
    flush()
    if offset != len(raw):
        raise ValueError('Unsupported XML text representation')
    for begin, end, replacement in sorted(edits, reverse=True):
        raw = raw[:begin] + replacement + raw[end:]
    ET.fromstring(raw + suffix)
    return raw.decode()


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


def http_payload_offset(text):
    """Return where an HTTP message body starts; bare payloads start at 0."""
    stripped = text.lstrip()
    if not (stripped.startswith('HTTP/') or re.match(r'[A-Za-z]+\s+[^\r\n]*\bHTTP/', stripped)):
        return 0
    leading = len(text) - len(stripped)
    line_break = r'(?:\r\n|\r(?!\n)|(?<!\r)\n)'
    separator = re.search(line_break + line_break, text[leading:])
    if not separator:
        raise ValueError('Unsupported HTTP body framing')
    return leading + separator.end()


def sanitize_bodies(s):
    raw = s.encode()
    parser = expat.ParserCreate(namespace_separator='}')
    stack = []
    spans = []

    def start(name, attrs):
        if stack:
            stack[-1]['nested'] = True
        if name.rsplit('}', 1)[-1] != 'body':
            return
        # XML parsing establishes identity; this scan only locates the end of
        # that already validated opening tag, respecting quoted attributes.
        match = re.match(rb'<(?:[^>\"\']|\"[^\"]*\"|\'[^\']*\')*>', raw[parser.CurrentByteIndex:])
        if not match:
            raise ValueError('Unsupported body opening tag')
        stack.append({'start': parser.CurrentByteIndex + match.end(), 'empty': match[0].endswith(b'/>'), 'text': [], 'nested': False})

    def end(name):
        if name.rsplit('}', 1)[-1] != 'body':
            return
        body = stack.pop()
        if body['nested']:
            raise ValueError('Nested body content is unsupported')
        if not body['empty']:
            spans.append((body['start'], parser.CurrentByteIndex, ''.join(body['text'])))

    def text(value):
        if stack:
            stack[-1]['text'].append(value)

    parser.StartElementHandler = start
    parser.EndElementHandler = end
    parser.CharacterDataHandler = text
    parser.Parse(raw, True)

    for begin, end, text in sorted(spans, reverse=True):
        offset = http_payload_offset(text)
        payload = re.sub(r'[ \t\r\n]', '', text[offset:])
        if not payload:
            continue
        try:
            cipher = base64.b64decode(payload, validate=True)
        except ValueError:
            if offset:
                raise ValueError('Unsupported HTTP body payload') from None
            continue  # Plain text stays; audit() rejects anything blob-like.
        if not cipher or len(cipher) % 16:
            if offset:
                raise ValueError('HTTP ciphertext is not block aligned')
            continue
        replacement = iter(base64.b64encode(bytes(len(cipher))).decode())
        position = 0

        def redact(segment, cdata=False):
            nonlocal position
            result = []
            token = r'\r\n|\r|[\s\S]' if cdata else r'&[^;]+;|\r\n|\r|[\s\S]'
            for match in re.finditer(token, segment):
                lexical = match[0]
                if lexical in ('\r', '\r\n'):
                    value = '\n'
                elif not cdata and lexical.startswith('&'):
                    if lexical.startswith('&#x'):
                        value = chr(int(lexical[3:-1], 16))
                    elif lexical.startswith('&#'):
                        value = chr(int(lexical[2:-1]))
                    else:
                        value = {'&amp;': '&', '&lt;': '<', '&gt;': '>', '&quot;': '"', '&apos;': "'"}[lexical]
                else:
                    value = lexical
                if position >= offset and value not in ' \t\r\n':
                    lexical = next(replacement)
                position += len(value)
                result.append(lexical)
            return ''.join(result)

        content = raw[begin:end].decode()
        parts = []
        for match in re.finditer(r'<!\[CDATA\[.*?\]\]>|<!--.*?-->|[^<]+', content, re.S):
            part = match[0]
            if part.startswith('<![CDATA['):
                parts.append('<![CDATA[' + redact(part[9:-3], True) + ']]>')
            elif part.startswith('<!--'):
                # Comments are outside semantic text and may contain secrets.
                parts.append('<!-- redacted -->')
            else:
                parts.append(redact(part))
        if position != len(text) or next(replacement, None) is not None:
            raise ValueError('Unsupported ciphertext representation')
        raw = raw[:begin] + ''.join(parts).encode() + raw[end:]
    return raw.decode()


def sanitize_free_text(s):
    raw = s.encode()
    parser = expat.ParserCreate(namespace_separator='}')
    spans, inside = [], []

    def start(name, attrs):
        if inside:
            raise ValueError('Markup inside free text is unsupported')
        if name in FREE_TEXT:
            opening = OPEN_TAG.match(raw, parser.CurrentByteIndex)
            inside.append(None if opening[0].endswith(b'/>') else opening.end())

    def end(name):
        if name in FREE_TEXT:
            begin = inside.pop()
            if begin is not None and begin != parser.CurrentByteIndex:
                spans.append((begin, parser.CurrentByteIndex))

    parser.StartElementHandler = start
    parser.EndElementHandler = end
    parser.Parse(raw, True)
    for begin, end in sorted(spans, reverse=True):
        raw = raw[:begin] + b'redacted' + raw[end:]
    return raw.decode()


def audit(raw, stream=False):
    """Fail closed if any value, attribute or comment still looks secret.

    Redaction rules only cover representations they recognise; this check
    does not trust them.
    """
    suffix = b'</' + re.match(rb'<([^\s/>]+)', raw)[1] + b'>' if stream else b''
    parser = expat.ParserCreate(namespace_separator='}')
    parser.ordered_attributes = True
    values, stack = [], []

    def start(name, attrs):
        stack.append((name, []))
        for key, value in zip(attrs[::2], attrs[1::2]):
            if (name, key) == CAPS_VER:
                continue
            # This numeric IQ correlation format is not a gateway identity.
            # The exception belongs to this protocol field, never provenance
            # or namespace values containing the same digits.
            if (name, key) == ('jabber:client}iq', 'id') and re.fullmatch(r'[0-9]{1,3}-[0-9]{9}', value):
                continue
            values.append(value)

    def end(name):
        _, parts = stack.pop()
        text = ''.join(parts)
        if name in SASL_PAYLOADS:
            payload = re.sub(r'\s', '', text)
            # Synthetic SASL values are only recognisable once decoded.
            text = '' if payload in ('', '=') else base64.b64decode(payload, validate=True).decode().replace('\x00', ' ')
        elif name.rsplit('}', 1)[-1] == 'body':
            offset = http_payload_offset(text)
            # Nothing outside printable ASCII may split a payload into
            # innocuous-looking pieces.
            values.append(re.sub(r'[^\x21-\x7e]', '', text[offset:]))
            text = text[:offset]
        values.append(text)

    parser.StartElementHandler = start
    parser.EndElementHandler = end
    parser.CharacterDataHandler = lambda text: stack and stack[-1][1].append(text)
    parser.CommentHandler = values.append
    parser.StartNamespaceDeclHandler = lambda prefix, uri: values.append(uri or '')
    parser.Parse(raw + suffix, True)
    audit_values(values)


def audit_form(value):
    # Audit equivalent spellings without rewriting public provenance or XML.
    # Nested escapes must converge within a fixed amount of work.
    for _ in range(8):
        try:
            decoded = html.unescape(urllib.parse.unquote(value, errors='strict'))
            decoded = unicodedata.normalize('NFKC', decoded)
            decoded = ''.join(str(unicodedata.digit(char)) if unicodedata.digit(char, None) is not None else char
                              for char in decoded if unicodedata.category(char) != 'Cf')
            decoded.encode('utf-8')
        except UnicodeError:
            raise ValueError('Unsupported audit value encoding') from None
        if decoded == value:
            return decoded
        value = decoded
    raise ValueError('Audit value encoding exceeds normalization limit')


def audit_values(values):
    for value in values:
        value = audit_form(value)
        if any(identity_replacement(match[0]).casefold() != match[0].casefold()
               for match in IDENTITY.finditer(value)):
            raise ValueError('Unredacted identity')
        value = SYNTHETIC.sub(' ', value)
        if IDENTITY.search(value):
            raise ValueError('Unredacted identity')
        if re.search(r'\d[^\s@/]*@', value):
            raise ValueError('Unredacted JID localpart')
        for digits in re.findall(r'(?<!\d)\d{9,}(?!\d)', value):
            significant = digits.lstrip('0')
            if len(digits) == 9 or significant and len(significant) <= 9:
                raise ValueError('Unredacted serial')
        # Recorder labels contain decimal timestamps, while padded serials
        # must already have failed above. Synthetic all-zero SASL proofs
        # remain covered by the existing blob exemption.
        value = re.sub(r'(?<![A-Za-z0-9])\d{13,20}(?![A-Za-z0-9])', 'timestamp', value)
        for run in BLOB.findall(value):
            if not re.fullmatch(r'A+=*|0+', run) and not all(WORD.fullmatch(part) for part in re.split(r'[/_-]', run) if part):
                raise ValueError('Unredacted base64 or hex value')


def audit_recording(recording):
    # Provenance is public too. Refuse private names rather than rewriting
    # them, since --recordings-from must still select the original files.
    audit_values([recording])


def audit_metadata(value, key=None):
    if isinstance(value, dict):
        for name, child in value.items():
            audit_metadata(child, name)
    elif isinstance(value, list):
        for child in value:
            audit_metadata(child)
    elif isinstance(value, str):
        if key == 'recording':
            audit_recording(value)
        elif key == 'file':
            if not re.fullmatch(r'(?:[0-9]{4}-[a-z-]+\.xml|stream-[0-9]+\.open)', value):
                raise ValueError('Unsafe public fixture filename')
        elif key in ('sha256', 'body_sha256'):
            # These are computed from sanitized bytes, not copied captures.
            if not re.fullmatch(r'[0-9a-f]{64}', value):
                raise ValueError('Invalid fixture digest')
        else:
            audit_values([value])


def sanitize(raw, exchange=None):
    s = raw.decode('utf-8')
    # Captured stream headers declare these defaults; standalone fixtures must
    # carry them. They are fixed here, not read from each original stream.
    name = re.match(r'<([^\s/>]+)', s)[1]
    opening = OPEN_TAG.match(raw)
    if not opening:
        raise ValueError('Unsupported stanza opening tag')
    attributes = {attr[1] for attr in ATTRIBUTE.finditer(opening[0])}
    if name == 'stream:features' and b'xmlns:stream' not in attributes:
        s = s.replace('<stream:features', '<stream:features xmlns:stream="http://etherx.jabber.org/streams"', 1)
    elif name in ('message','iq','presence') and b'xmlns' not in attributes:
        s = s.replace('<'+name, '<'+name+' xmlns="jabber:client"', 1)
    s = sanitize_identities(s)
    s = sanitize_bodies(s)
    s = sanitize_free_text(s)
    s = sanitize_sasl(s, exchange if exchange is not None else {})
    ET.fromstring(s)  # Fail closed on invalid redaction or incomplete XML.
    audit(s.encode())
    return s.encode(), name

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('captures', type=Path)
    p.add_argument('output', type=Path)
    p.add_argument('--recordings-from', type=Path, help='Restrict rebuilding to recordings indexed by an existing manifest; - reads that manifest from stdin')
    a = p.parse_args()
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
    if a.recordings_from is not None:
        previous_manifest = json.load(sys.stdin) if str(a.recordings_from) == '-' else json.loads(a.recordings_from.read_text())
        recordings = {occurrence['recording'] for entry in previous_manifest for occurrence in entry['occurrences']}
        sources = [source for source in sources if source.relative_to(a.captures).as_posix() in recordings]
        if {source.relative_to(a.captures).as_posix() for source in sources} != recordings:
            raise ValueError('Indexed recordings are missing; existing fixtures were not changed')
    if not sources:
        raise ValueError('No capture files found; existing fixtures were not changed')
    for source in sources:
        audit_recording(source.relative_to(a.captures).as_posix())
    for source in sources:
        raw = source.read_bytes()
        exchange = {}
        for candidate in re.finditer(rb'<(?:[^\s<>/:]+:)?stream(?=[\s/>])', raw):
            opening = OPEN_TAG.match(raw, candidate.start())
            if not opening:
                raise ValueError('Unsupported stream header spelling')
            header = sanitize_identities(opening[0].decode(), stream=True)
            audit(header.encode(), stream=True)
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
    if not manifest:
        raise ValueError('No complete XMPP stanzas found; existing fixtures were not changed')
    audit_metadata(manifest)
    audit_metadata(streams)
    outputs['manifest.json'] = (json.dumps(manifest, indent=2)+'\n').encode()
    outputs['streams.json'] = (json.dumps(streams, indent=2)+'\n').encode()
    a.output.mkdir(parents=True, exist_ok=True)
    for filename, data in outputs.items():
        (a.output / filename).write_bytes(data)
    for filename in previous:
        if filename not in outputs:
            (a.output / filename).unlink(missing_ok=True)
    print(f'{len(manifest)} distinct sanitized stanzas from {len(sources)} recordings')


if __name__ == '__main__':
    main()
