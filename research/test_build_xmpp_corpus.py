"""XML identities and credentials must be redacted without lexical drift."""
import base64
import hashlib
import importlib.util
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest
import xml.etree.ElementTree as ET

SCRIPT = Path(__file__).with_name('build-xmpp-corpus.py')
spec = importlib.util.spec_from_file_location('corpus', SCRIPT)
corpus = importlib.util.module_from_spec(spec)
spec.loader.exec_module(corpus)
CORPUS = SCRIPT.parent.parent / 'protocol/testdata/xmpp'


class SanitizerTests(unittest.TestCase):
    def test_identity_spellings_end_to_end(self):
        serial = '987654321'
        adapter = '1780000000123'
        uuid = 'a1b2c3d4-e5f6-7890-abcd-0123456789ef'
        for encode in (lambda value: value,
                       lambda value: ''.join(f'&#{ord(c)};' for c in value),
                       lambda value: ''.join(f'&#x{ord(c):X};' for c in value)):
            for prefix in ('stream:', 's:', ''):
                with self.subTest(prefix=prefix, serial=encode(serial)), tempfile.TemporaryDirectory() as directory:
                    captures, output = Path(directory)/'captures', Path(directory)/'output'
                    captures.mkdir()
                    ns = f'xmlns:{prefix[:-1]}' if prefix else 'xmlns'
                    gateway = encode('rrcgateway_' + serial)
                    contact = encode('rrccontact_' + serial)
                    resource = encode('xmpp-adapter_' + adapter)
                    encoded_uuid = encode(uuid)
                    private_id = 'synthetic>stream&amp;id'
                    stream_namespace = corpus.STREAM_NS.replace('streams', encode('streams'))
                    header = f'<{prefix}stream {ns} = "{stream_namespace}" id \t=\r\n\'{private_id}\' from = "{gateway}@fixture.example/{encoded_uuid}">'
                    iq = f'<iq xmlns = "jabber:client" xmlns:b = "urn:ietf:params:xml:ns:xmpp-bind" from = \'{contact}@fixture.example/{resource}\' note="unchanged &#x41;&amp;&gt;"><b:bind><b:jid> \r\n{gateway}@fixture.example/<![CDATA[{uuid}]]>\t</b:jid></b:bind></iq>'
                    plain = '<message xmlns="jabber:client"><body>plain &#x41;\r\n<![CDATA[ note ]]>&amp;</body></message>'
                    cipher = base64.b64encode(bytes(range(16))).decode()
                    encrypted = '<message xmlns="jabber:client"><body>HTTP/1.0 200 OK&#13;&#13;' + encode(cipher) + '</body></message>'
                    proof = self.stanza('auth', b'\x00synthetic-user\x00synthetic-password', 'mechanism="PLAIN"', True, True).decode()
                    (captures/'synthetic.xmpp').write_bytes((header + iq + plain + encrypted + proof).encode())
                    result = subprocess.run([sys.executable, str(SCRIPT), str(captures), str(output)], capture_output=True, check=False)
                    self.assertEqual(result.returncode, 0, result.stderr.decode())
                    streams = json.loads((output/'streams.json').read_text())
                    cleaned_header = (output/streams[0]['file']).read_bytes()
                    expected_header = header.replace(private_id, 'fixture-stream').replace(gateway, 'rrcgateway_123456789').replace(encoded_uuid, '00000000-0000-4000-8000-000000000000')
                    self.assertEqual(cleaned_header, expected_header.encode())
                    root = ET.fromstring(cleaned_header + f'</{prefix}stream>'.encode())
                    self.assertEqual(root.tag, f'{{{corpus.STREAM_NS}}}stream')
                    self.assertEqual(root.attrib['id'], 'fixture-stream')
                    manifest = json.loads((output/'manifest.json').read_text())
                    self.assertEqual(len(manifest), 4)
                    cleaned = [(entry, (output/entry['file']).read_bytes()) for entry in manifest]
                    expected_iq = iq.replace(contact, 'rrccontact_123456789').replace(resource, 'xmpp-adapter_1000000000000').replace(gateway, 'rrcgateway_123456789').replace(uuid, '00000000-0000-4000-8000-000000000000')
                    self.assertEqual(cleaned[0][1], expected_iq.encode())
                    self.assertEqual(cleaned[1][1], plain.encode())
                    body = ET.fromstring(cleaned[2][1]).find('{jabber:client}body').text
                    self.assertEqual(base64.b64decode(body.split('\r\r')[1], validate=True), bytes(16))
                    self.assertEqual(self.decoded(cleaned[3][1]), b'\x00fixture-user\x00fixture-password')
                    for entry, raw in cleaned:
                        self.assertEqual(hashlib.sha256(raw).hexdigest(), entry['sha256'])
                        element = ET.fromstring(raw)
                        for node in element.iter():
                            values = list(node.attrib.values()) + [node.text or '', node.tail or '']
                            for value in values:
                                for original in (serial, adapter, uuid, 'synthetic-user', 'synthetic-password'):
                                    self.assertNotIn(original, value)

    def test_identity_boundaries_preserve_lexical_structure(self):
        raw = b'<iq xmlns="jabber:client" xmlns:b="urn:ietf:params:xml:ns:xmpp-bind" note="\t&#9;&#x41;&amp;\r\n"><b:bind><b:jid>rrcgate<![CDATA[way_98]]><!-- join -->&#55;654321@fixture.example/a1b2c3d4-<![CDATA[e5f6-7890]]>-abcd-0123456789ef</b:jid></b:bind>\r\n</iq>'
        expected = b'<iq xmlns="jabber:client" xmlns:b="urn:ietf:params:xml:ns:xmpp-bind" note="\t&#9;&#x41;&amp;\r\n"><b:bind><b:jid>rrcgateway_123456789<![CDATA[]]><!-- join -->@fixture.example/00000000-0000-4000-8000-000000000000<![CDATA[]]></b:jid></b:bind>\r\n</iq>'
        cleaned, _ = corpus.sanitize(raw)
        self.assertEqual(cleaned, expected)
        self.assertEqual(ET.fromstring(cleaned).find('.//{urn:ietf:params:xml:ns:xmpp-bind}jid').text,
                         'rrcgateway_123456789@fixture.example/00000000-0000-4000-8000-000000000000')

    def test_unicode_offsets_and_namespaced_attributes(self):
        raw = '<iq xmlns="jabber:client" xmlns:e="urn:extension" e:jid="π 😀 rrccontact_&#57;87654321" note="π &#x41;">😀 rrcgateway_98&#55;654321 <e:resource>xmpp-adapter_1780000000&#49;23</e:resource></iq>'
        expected = raw.replace('rrccontact_&#57;87654321', 'rrccontact_123456789').replace('rrcgateway_98&#55;654321', 'rrcgateway_123456789').replace('xmpp-adapter_1780000000&#49;23', 'xmpp-adapter_1000000000000')
        cleaned, _ = corpus.sanitize(raw.encode())
        self.assertEqual(cleaned, expected.encode())

    def test_missing_indexed_input_preserves_all_outputs(self):
        with tempfile.TemporaryDirectory() as directory:
            captures, output = Path(directory)/'captures', Path(directory)/'output'
            captures.mkdir()
            output.mkdir()
            previous = {
                '0000-message.xml': b'previous sanitized fixture',
                'manifest.json': json.dumps([{'file': '0000-message.xml', 'occurrences': [{'recording': 'missing.xmpp'}]}]).encode(),
                'stream-00.open': b'previous sanitized header',
                'streams.json': json.dumps([{'file': 'stream-00.open'}]).encode(),
            }
            for filename, raw in previous.items():
                (output/filename).write_bytes(raw)
            (captures/'unindexed.xmpp').write_text('<message><body>plain notification</body></message>')
            result = subprocess.run([sys.executable, str(SCRIPT), str(captures), str(output), '--recordings-from', str(output/'manifest.json')], capture_output=True, check=False)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual({path.name: path.read_bytes() for path in output.iterdir()}, previous)

    def test_invalid_stream_headers_preserve_all_outputs(self):
        for header in (
            '<stream:stream id = "synthetic-private">',
            '<stream:stream xmlns:stream="urn:other" id = "synthetic-private">',
            f'<stream:stream xmlns:stream="{corpus.STREAM_NS}" id = unquoted>',
            f'<stream:stream xmlns:stream="{corpus.STREAM_NS}" id="one" id = "two">',
            f'<stream:stream xmlns:stream="{corpus.STREAM_NS}" id = "">',
            f'<stream:stream xmlns:stream="{corpus.STREAM_NS}" id = "&unknown;">',
            f'<stream:stream xmlns:stream="{corpus.STREAM_NS}" id = "unterminated>',
            f'<stream:stream xmlns:stream="{corpus.STREAM_NS}"/>',
            f'<stream:stream xmlns:stream="{corpus.STREAM_NS}" xmlns:private="urn:rrcgateway_&#57;87654321" id="one">',
            f'<stream:stream xmlns:stream="{corpus.STREAM_NS}" from="987654321@fixture.example" id="one">',
        ):
            with self.subTest(header=header), tempfile.TemporaryDirectory() as directory:
                captures, output = Path(directory)/'captures', Path(directory)/'output'
                captures.mkdir()
                output.mkdir()
                previous = {
                    '0000-message.xml': b'previous sanitized fixture',
                    'manifest.json': json.dumps([{'file': '0000-message.xml'}]).encode(),
                    'stream-00.open': b'previous sanitized header',
                    'streams.json': json.dumps([{'file': 'stream-00.open'}]).encode(),
                }
                for filename, raw in previous.items():
                    (output/filename).write_bytes(raw)
                (captures/'synthetic.xmpp').write_text(header + '<message><body>plain notification</body></message>')
                result = subprocess.run([sys.executable, str(SCRIPT), str(captures), str(output)], capture_output=True, check=False)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual({path.name: path.read_bytes() for path in output.iterdir()}, previous)

    def test_private_recording_names_fail_before_mutation(self):
        recording = (f'<stream:stream xmlns="jabber:client" xmlns:stream="{corpus.STREAM_NS}" '
                     'from="rrcgateway_987654321" to="fixture.example">'
                     '<message from="rrcgateway_987654321@fixture.example"><body>plain</body></message>')
        for name in (
            'rrcgateway_987654321.bin',
            'rrcgateway_1234567890.xml',
            'xmpp-adapter_10000000000000.xmpp',
            'RRCCONTACT_987654321.xml',
            'xmpp-adapter_1780000000123.xmpp',
            '987654321.xmpp',
            's987654321.xmpp',
            '0987654321.xmpp',
            '0000000000987654321.xmpp',
            'rrcgateway_&#57;87654321.xmpp',
            'rrcgateway_&#x39;87654321.xmpp',
            'rrcgateway_&amp;#57;87654321.xmpp',
            'rrcgateway_%3987654321.xmpp',
            'rrcgateway_%253987654321.xmpp',
            'rrcgateway_%26%2357%3B87654321.xmpp',
            'ｒｒｃｇａｔｅｗａｙ_９８７６５４３２１.xmpp',
            '０９８７６５４３２１.xmpp',
            '٠٩٨٧٦٥٤٣٢١.xmpp',
            'rrcgate\u200bway_98\u200b7654321.xmpp',
            '987654321@fixture.example.xmpp',
            'private/a1b2c3d4-e5f6-7890-abcd-0123456789ef/session.xmpp',
            'private/a1b2c3d4e5f67890abcd0123456789ef/session.xmpp',
        ):
            for existing in (False, True):
                with self.subTest(name=name, existing=existing), tempfile.TemporaryDirectory() as directory:
                    captures, output = Path(directory)/'captures', Path(directory)/'output'
                    source = captures/name
                    source.parent.mkdir(parents=True)
                    source.write_text(recording)
                    previous = {
                        '0000-message.xml': b'previous sanitized fixture',
                        'manifest.json': json.dumps([{'file': '0000-message.xml'}]).encode(),
                        'stream-00.open': b'previous sanitized header',
                        'streams.json': json.dumps([{'file': 'stream-00.open'}]).encode(),
                    }
                    if existing:
                        output.mkdir()
                        for filename, raw in previous.items():
                            (output/filename).write_bytes(raw)
                    result = subprocess.run([sys.executable, str(SCRIPT), str(captures), str(output)], capture_output=True, check=False)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertNotIn(name.encode(), result.stderr)
                    if existing:
                        self.assertEqual({path.name: path.read_bytes() for path in output.iterdir()}, previous)
                    else:
                        self.assertFalse(output.exists())

    def test_public_metadata_audit(self):
        for recording in ('rrcgateway_987654321.xmpp', '987654321.xmpp', '0987654321.xmpp',
                          'rrcgateway_&#57;87654321.xmpp', 'rrcgateway_%253987654321.xmpp',
                          'private/a1b2c3d4e5f67890abcd0123456789ef/session.xmpp'):
            for metadata in (
                [{'file': '0000-message.xml', 'occurrences': [{'recording': recording, 'offset': 0, 'bytes': 1}]}],
                [{'file': 'stream-00.open', 'recording': recording, 'offset': 0}],
            ):
                with self.subTest(recording=recording, index=metadata[0]['file']), self.assertRaises(ValueError):
                    corpus.audit_metadata(metadata)
        for filename in ('manifest.json', 'streams.json'):
            corpus.audit_metadata(json.loads((CORPUS/filename).read_text()))
        for namespace in ('urn:device:987654321', 'urn:device:0987654321', 'urn:device:%253987654321'):
            with self.subTest(namespace=namespace), self.assertRaises(ValueError):
                corpus.audit_metadata([{'expected': {'namespace': namespace}}])

    def test_namespace_serials_fail_before_mutation(self):
        for namespace in ('urn:device:987654321', 'urn:device:0987654321', 'urn:device:000000987654321',
                          'urn:device:&#57;87654321', 'urn:device:&#x39;87654321',
                          'urn:device:%253987654321', 'urn:device:９８７６５４３２１'):
            for recording in (
                f'<iq xmlns="{namespace}"/>',
                f'<iq xmlns="jabber:client"><query xmlns="{namespace}"/></iq>',
                f'<stream:stream xmlns:stream="{corpus.STREAM_NS}" xmlns:device="{namespace}"><iq xmlns="jabber:client"/>',
            ):
                for existing in (False, True):
                    with self.subTest(namespace=namespace, existing=existing), tempfile.TemporaryDirectory() as directory:
                        captures, output = Path(directory)/'captures', Path(directory)/'output'
                        captures.mkdir()
                        (captures/'synthetic.xmpp').write_text(recording)
                        previous = {
                            '0000-iq.xml': b'previous sanitized fixture',
                            'manifest.json': json.dumps([{'file': '0000-iq.xml'}]).encode(),
                            'stream-00.open': b'previous sanitized header',
                            'streams.json': json.dumps([{'file': 'stream-00.open'}]).encode(),
                        }
                        if existing:
                            output.mkdir()
                            for filename, raw in previous.items():
                                (output/filename).write_bytes(raw)
                        result = subprocess.run([sys.executable, str(SCRIPT), str(captures), str(output)], capture_output=True, check=False)
                        self.assertNotEqual(result.returncode, 0)
                        self.assertNotIn(namespace.encode(), result.stderr)
                        if existing:
                            self.assertEqual({path.name: path.read_bytes() for path in output.iterdir()}, previous)
                            self.assertTrue(all(b'987654321' not in raw for raw in previous.values()))
                        else:
                            self.assertFalse(output.exists())

    def test_shared_serial_audit_and_normalization_bound(self):
        for value in ('987654321', 's987654321', 'urn:device:s987654321', '0987654321', '0000000000987654321', 'urn:device:&#57;87654321',
                      'urn:device:%253987654321', 'urn:device:٠٩٨٧٦٥٤٣٢١'):
            with self.subTest(value=value), self.assertRaises(ValueError):
                corpus.audit_values([value])
        for length in range(13, 21):
            corpus.audit_values(['1' + '7'*(length-1)])
        corpus.audit_values([f'rrcgateway_{corpus.SERIAL}', f'xmpp-adapter_{corpus.ADAPTER}', corpus.UUID,
                             'rspauth=' + '0'*32, 'nc=00000001'])
        encoded = '%2e'
        for _ in range(12):
            encoded = encoded.replace('%', '%25')
        with self.assertRaises(ValueError):
            corpus.audit_recording('capture' + encoded + '.xmpp')
        with self.assertRaises(ValueError):
            corpus.audit_recording('capture-%FF.xmpp')

    def test_iq_correlation_exception_is_field_scoped(self):
        raw = b'<iq xmlns="jabber:client" id="100-876543219"><query xmlns="urn:fixture"/></iq>'
        self.assertEqual(corpus.sanitize(raw)[0], raw)
        for raw in (
            '<iq xmlns="jabber:client" id="987654321"/>',
            '<iq xmlns="jabber:client" id="100-0987654321"/>',
            '<iq xmlns="jabber:client" note="100-876543219"/>',
            '<iq xmlns="urn:device:100-876543219"/>',
        ):
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                corpus.sanitize(raw.encode())
        with self.assertRaises(ValueError):
            corpus.audit_recording('100-876543219.xmpp')

    def test_safe_provenance_and_recording_selection(self):
        with tempfile.TemporaryDirectory() as directory:
            # The private capture root is never published; relative names are.
            captures = Path(directory)/'rrcgateway_987654321'
            sources = (
                '1780000000123-device.bin', '1780000000123-cloud.bin',
                'offline-1780000000123-device.bin', 'offline-1780000000123-status-response.xml',
                'offline-status-response.xml', 'offline-device.bin', 'local-status-response.xml',
                '1780000000123-local-status-response.xml', 'completed-local-status-request.xml',
                'offline/1780000000123456789/device-to-server.xmpp', 'offline/1780000000123456789/server-to-device.xmpp',
                'both/1780000000123456789/device-to-server.xmpp', 'both/1780000000123456789/server-to-device.xmpp',
                'cloud-followup/1780000000123456789/device-to-server.xmpp', 'cloud-followup/1780000000123456789/server-to-device.xmpp',
                'sam%70le.xmpp', 'sam&#112;le.xmpp',
            )
            for name in sources:
                source = captures/name
                source.parent.mkdir(parents=True, exist_ok=True)
                source.write_text(f'<stream:stream xmlns="jabber:client" xmlns:stream="{corpus.STREAM_NS}" '
                                  'from="rrcgateway_987654321" to="fixture.example">'
                                  '<message from="rrcgateway_987654321@fixture.example"><body>plain</body></message>')
            output = Path(directory)/'output'
            command = [sys.executable, str(SCRIPT), str(captures), str(output)]
            subprocess.run(command, capture_output=True, check=True)
            original = {path.name: path.read_bytes() for path in output.iterdir()}
            self.assertTrue(all(b'987654321' not in raw for raw in original.values()))
            manifest = json.loads(original['manifest.json'])
            self.assertEqual({occurrence['recording'] for entry in manifest for occurrence in entry['occurrences']}, set(sources))
            self.assertTrue(json.loads(original['streams.json']))
            subprocess.run(command + ['--recordings-from', str(output/'manifest.json')], capture_output=True, check=True)
            self.assertEqual({path.name: path.read_bytes() for path in output.iterdir()}, original)
            subprocess.run(command + ['--recordings-from', '-'], input=original['manifest.json'], capture_output=True, check=True)
            self.assertEqual({path.name: path.read_bytes() for path in output.iterdir()}, original)

    def test_xml_declarations_and_entities_fail_closed(self):
        for raw in (
            '<!DOCTYPE iq [<!ENTITY identity "rrcgateway_987654321">]><iq>&identity;</iq>',
            '<iq xmlns="jabber:client"><?private identity?><jid>rrcgateway_987654321</jid></iq>',
            '<iq xmlns="jabber:client"><jid>&undefined;</jid></iq>',
        ):
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                corpus.sanitize_identities(raw)

    def test_committed_fixtures_are_fixed_points(self):
        # Globbed rather than indexed: an unindexed file is still published.
        stanzas, headers = sorted(CORPUS.glob('*.xml')), sorted(CORPUS.glob('*.open'))
        self.assertTrue(stanzas and headers)
        for path in stanzas:
            raw = path.read_bytes()
            with self.subTest(file=path.name):
                self.assertEqual(corpus.sanitize(raw)[0], raw)
        for path in headers:
            raw = path.read_bytes()
            with self.subTest(file=path.name):
                self.assertEqual(corpus.sanitize_identities(raw.decode(), stream=True).encode(), raw)
                corpus.audit(raw, stream=True)

    def test_ciphertext_sizes_and_xml_spellings(self):
        for size in (16, 32, 48, 256):
            cipher = base64.b64encode(bytes((i % 255) + 1 for i in range(size))).decode()
            for prefix in ('', 'PUT /resource HTTP/1.1\r\nContent-Type: application/json\r\n\r\n', 'HTTP/1.0 200 OK\r\n\r\n'):
                for spelling in ('text', 'cdata', 'entities', 'wrapped', 'comment'):
                    with self.subTest(size=size, prefix=bool(prefix), spelling=spelling):
                        payload = cipher
                        if spelling == 'entities':
                            payload = ''.join(f'&#{ord(c)};' for c in cipher)
                        elif spelling == 'wrapped':
                            payload = '\n '.join(cipher[i:i+4] for i in range(0, len(cipher), 4)) + '\n'
                        elif spelling == 'comment':
                            payload = cipher[:4] + '<!-- private-comment -->' + cipher[4:]
                        content = prefix + payload
                        if spelling == 'cdata':
                            content = ' \n<![CDATA[' + content + ']]>\n '
                        raw = f'<message xmlns="jabber:client"><b:body xmlns:b="jabber:client" note="greater&gt;sign">{content}</b:body></message>'
                        cleaned, _ = corpus.sanitize(raw.encode())
                        text = ET.fromstring(cleaned).find('{jabber:client}body').text
                        if prefix:
                            self.assertTrue(text.lstrip().startswith(prefix.replace('\r\n', '\n')))
                            text = re.split(r'\r?\n\r?\n', text, maxsplit=1)[1]
                        decoded = base64.b64decode(''.join(text.split()), validate=True)
                        self.assertEqual(decoded, bytes(size))
                        self.assertNotIn(b'private-comment', cleaned)
                        if spelling == 'cdata':
                            self.assertIn(b'<![CDATA[', cleaned)

    def test_body_controls_and_unsupported_shapes(self):
        for raw in (
            '<message><body>HTTP/1.0 204 No Content\r\n\r\n</body></message>',
            '<message><body>plain notification</body></message>',
            '<message><body/></message>',
        ):
            cleaned, _ = corpus.sanitize(raw.encode())
            self.assertEqual(ET.fromstring(cleaned).find('{jabber:client}body').text,
                             ET.fromstring(raw).find('body').text)
        for raw in (
            '<message><body><x>opaque</x></body></message>',
            '<message><body>HTTP/1.0 200 OK\n\nnot-a-supported-ciphertext</body></message>',
            '<message><body>HTTP/1.0 200 OK\n\nc2VjcmV0</body></message>',
            '<message><body>HTTP/1.0 200 OK\nciphertext-without-header-separator</body></message>',
        ):
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                corpus.sanitize(raw.encode())

    def test_committed_bodies_are_zeroed(self):
        # Every payload, aligned or not, must be absent or base64 zero bytes.
        for path in sorted(CORPUS.glob('*.xml')):
            for body in ET.fromstring(path.read_bytes()).iter():
                if body.tag.rsplit('}', 1)[-1] != 'body':
                    continue
                text = body.text or ''
                payload = ''.join(text[corpus.http_payload_offset(text):].split())
                with self.subTest(file=path.name):
                    self.assertFalse(any(base64.b64decode(payload, validate=True)))

    def test_unrecognised_values_fail_closed(self):
        secret = hashlib.sha256(b'synthetic secret').digest()
        aligned = base64.b64encode(secret[:16]).decode()
        zero = base64.b64encode(bytes(16)).decode()
        for case, raw in {
            '20-byte blob': f'<message><body>{base64.b64encode(secret[:20]).decode()}</body></message>',
            'unpadded': f'<message><body>{aligned.rstrip("=")}</body></message>',
            'url-safe': f'<message><body>{base64.urlsafe_b64encode(bytes(range(240, 256))).decode()}</body></message>',
            'prefixed': f'<message><body>v1:{aligned}</body></message>',
            'U+2028': f'<message><body>{aligned[:8]}\u2028{aligned[8:]}</body></message>',
            'zero-width split': f'<message><body>{aligned[:8]}\u200b{aligned[8:16]}\u200b{aligned[16:]}</body></message>',
            'subject': f'<message><subject>{aligned}</subject><body>plain</body></message>',
            'attribute': f'<message xmlns="jabber:client" note="{aligned}"><body>plain</body></message>',
            'comment': f'<message xmlns="jabber:client"><!-- {aligned} --><body>plain</body></message>',
            'second body': f'<message><body>{zero}</body><body xml:lang="de">{base64.b64encode(secret[:20]).decode()}</body></message>',
            'hex': f'<iq xmlns="jabber:client"><query xmlns="urn:fixture">{secret[:8].hex()}</query></iq>',
            'bare-serial JID': '<message to="987654321@fixture.example"><body>plain</body></message>',
            'unhyphenated UUID': '<iq xmlns="jabber:client"><jid>rrcgateway_123456789@fixture.example/a1b2c3d4e5f67890abcd0123456789ef</jid></iq>',
            'free text markup': f'<failure xmlns="{corpus.SASL_NS}"><text>private <b>markup</b></text></failure>',
            'namespace URI': f'<iq xmlns="jabber:client"><q xmlns:x="urn:{secret[:16].hex()}"/></iq>',
            'element name': '<iq xmlns="jabber:client"><rrcgateway_987654321 xmlns="urn:x"/></iq>',
            'attribute name': '<iq xmlns="jabber:client"><q xmlns="urn:x" rrcgateway_987654321="v"/></iq>',
            'namespace prefix': '<iq xmlns="jabber:client"><q xmlns:rrcgateway_987654321="urn:x"/></iq>',
            'bare serial attribute name': '<iq xmlns="jabber:client" s987654321="safe"/>',
            'bare serial namespace prefix': '<iq xmlns="jabber:client"><q xmlns:s987654321="urn:x"/></iq>',
        }.items():
            with self.subTest(case=case), self.assertRaises(ValueError):
                corpus.sanitize(raw.encode())
        # Decoded SASL directives the rewrite does not know are audited too.
        for kind, payload in [('challenge', f'nonce="fixture",qop="{secret[:16].hex()}",charset=utf-8,algorithm=md5-sess'),
                              ('response', f'username="u",nonce="n",cnonce="c",nc={secret[:16].hex()},qop=auth,digest-uri="xmpp/x",response={"0" * 32}')]:
            with self.subTest(case=kind), self.assertRaises(ValueError):
                corpus.sanitize(self.stanza(kind, payload.encode()), {'mechanism': 'DIGEST-MD5'})

    def test_unsafe_previous_manifest_deletes_nothing(self):
        with tempfile.TemporaryDirectory() as directory:
            captures, output = Path(directory)/'captures', Path(directory)/'output'
            captures.mkdir()
            output.mkdir()
            victim = Path(directory)/'victim'
            victim.write_bytes(b'outside the corpus')
            (output/'manifest.json').write_text(json.dumps([{'file': '../victim'}]))
            (captures/'synthetic.xmpp').write_text('<message><body>plain notification</body></message>')
            result = subprocess.run([sys.executable, str(SCRIPT), str(captures), str(output)], capture_output=True, check=False)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(victim.read_bytes(), b'outside the corpus')

    def test_research_scripts_start(self):
        # Most are imported by no test, so --help is what catches a syntax error.
        for script in ('record-xmpp.py', 'xmpp-relay.py', 'offline-status-probe.py', 'capture-device.py'):
            with self.subTest(script=script):
                result = subprocess.run([sys.executable, str(SCRIPT.with_name(script)), '--help'], capture_output=True, check=False)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_every_body_is_zeroed(self):
        cipher = base64.b64encode(bytes(range(1, 33))).decode()
        cleaned, _ = corpus.sanitize(f'<message><body>{cipher}</body><body xml:lang="de">HTTP/1.0 200 OK\n\n{cipher}</body></message>'.encode())
        bodies = [body.text for body in ET.fromstring(cleaned) if body.tag.endswith('body')]
        self.assertEqual(bodies, [base64.b64encode(bytes(32)).decode(), 'HTTP/1.0 200 OK\n\n' + base64.b64encode(bytes(32)).decode()])

    def test_free_text_is_redacted(self):
        for raw, private in (
            (f'<failure xmlns="{corpus.SASL_NS}"><not-authorized/><text xml:lang="en">user private-login, password private-password</text></failure>',
             ['user private-login, password private-password']),
            ('<iq xmlns="jabber:client" type="result" id="v1"><query xmlns="jabber:iq:version"><name>private-host</name><version>02.22.00</version><os>Linux private-kernel</os></query></iq>',
             ['private-host', 'Linux private-kernel']),
            ('<message xmlns="jabber:client" type="error"><error type="cancel"><text xmlns="urn:ietf:params:xml:ns:xmpp-stanzas">private-reason</text></error></message>',
             ['private-reason']),
            ('<iq xmlns="jabber:client" type="set" id="a1"><query xmlns="jabber:iq:auth"><password>private-password</password><digest>private-digest</digest></query></iq>',
             ['private-password', 'private-digest']),
            ('<iq xmlns="jabber:client" type="set" id="r1"><query xmlns="jabber:iq:register"><password>private-password</password></query></iq>',
             ['private-password']),
        ):
            expected = raw
            for value in private:
                expected = expected.replace(value, 'redacted')
            with self.subTest(raw=raw):
                self.assertEqual(corpus.sanitize(raw.encode())[0], expected.encode())

    def stanza(self, kind, payload, attrs='', cdata=False, whitespace=False):
        text = base64.b64encode(payload).decode()
        if whitespace:
            text = '\n  ' + text[:4] + '\n' + text[4:] + '\t\n'
        if cdata:
            text = '<![CDATA[' + text + ']]>'
        return f'<{kind} xmlns="{corpus.SASL_NS}" {attrs}>{text}</{kind}>'.encode()

    def decoded(self, cleaned):
        text = ET.fromstring(cleaned).text or ''
        return base64.b64decode(''.join(text.split()), validate=True)

    def test_plain_initial_and_continuation(self):
        for authorization in (b'', b'private-authorized-user'):
            secret = authorization + b'\x00private-login\x00private-password'
            for kind in ('auth', 'response'):
                for cdata in (False, True):
                    with self.subTest(kind=kind, cdata=cdata):
                        state = {'mechanism': 'PLAIN'}
                        attrs = 'mechanism="PLAIN"' if kind == 'auth' else ''
                        cleaned, _ = corpus.sanitize(self.stanza(kind, secret, attrs, cdata, True), state)
                        decoded = self.decoded(cleaned)
                        self.assertEqual(len(decoded.split(b'\x00')), 3)
                        for value in (b'private-authorized-user', b'private-login', b'private-password'):
                            self.assertNotIn(value, decoded)
                        if cdata:
                            self.assertIn(b'<![CDATA[', cleaned)

    def test_digest_proofs_and_identifiers(self):
        payload = b'username="private-user",nonce="private-nonce",cnonce="private-cnonce",response=abcdef0123456789abcdef0123456789,authzid="private-auth",digest-uri="xmpp/private-host",nc=00000001,qop=auth'
        for kind, value in [('response', payload), ('challenge', b'nonce="private-nonce",qop="auth,auth-int",charset=utf-8,algorithm=md5-sess'), ('success', b'rspauth=abcdef0123456789abcdef0123456789')]:
            for cdata in (False, True):
                cleaned, _ = corpus.sanitize(self.stanza(kind, value, cdata=cdata, whitespace=True), {'mechanism': 'DIGEST-MD5'})
                decoded = self.decoded(cleaned)
                self.assertNotIn(b'private-', decoded)
                self.assertNotIn(b'abcdef0123456789abcdef0123456789', decoded)

    def test_unsupported_payloads_fail_closed(self):
        for raw in (
            f'<auth xmlns="{corpus.SASL_NS}" mechanism="PLAIN">%%%bad</auth>',
            f'<auth xmlns="{corpus.SASL_NS}" mechanism="UNKNOWN">c2VjcmV0</auth>',
            f'<auth xmlns="{corpus.SASL_NS}" mechanism="PLAIN">c2VjcmV0</auth>',
            f'<response xmlns="{corpus.SASL_NS}">c2VjcmV0</response>',
            '<auth xmlns="urn:other" mechanism="PLAIN">c2VjcmV0</auth>',
            f'<response xmlns="{corpus.SASL_NS}"><x>c2VjcmV0</x></response>',
        ):
            with self.subTest(raw=raw):
                with self.assertRaises(ValueError):
                    corpus.sanitize(raw.encode())

    def test_nested_sasl_payloads_fail_closed(self):
        for wrapper in ('iq', 'message', 'presence'):
            for kind in ('auth', 'response', 'challenge', 'success'):
                payload = self.stanza(kind, b'\x00private-user\x00private-password', 'mechanism="PLAIN"')
                raw = b'<' + wrapper.encode() + b'>' + payload + b'</' + wrapper.encode() + b'>'
                with self.subTest(wrapper=wrapper, kind=kind):
                    with self.assertRaises(ValueError):
                        corpus.sanitize(raw)

    def test_empty_continuations_and_mechanism_state(self):
        state = {}
        for kind in ('auth', 'response', 'success'):
            attrs = 'mechanism="PLAIN"' if kind == 'auth' else ''
            raw = f'<{kind} xmlns="{corpus.SASL_NS}" {attrs}/>'.encode()
            cleaned, _ = corpus.sanitize(raw, state)
            self.assertIsNone(ET.fromstring(cleaned).text)
        self.assertEqual(state['mechanism'], 'PLAIN')

    def test_failed_build_preserves_previous_outputs(self):
        with tempfile.TemporaryDirectory() as directory:
            captures = Path(directory)/'captures'
            output = Path(directory)/'output'
            captures.mkdir()
            output.mkdir()
            filename = '0000-auth.xml'
            original = b'previous sanitized fixture'
            (output/filename).write_bytes(original)
            manifest = json.dumps([{'file': filename}]).encode()
            (output/'manifest.json').write_bytes(manifest)
            (captures/'sample.xml').write_text(f'<auth xmlns="{corpus.SASL_NS}" mechanism="PLAIN">%%%bad</auth>')
            result = subprocess.run([sys.executable, str(SCRIPT), str(captures), str(output)], capture_output=True, check=False)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual((output/filename).read_bytes(), original)
            self.assertEqual((output/'manifest.json').read_bytes(), manifest)

    def test_empty_input_preserves_previous_outputs(self):
        with tempfile.TemporaryDirectory() as directory:
            captures = Path(directory)/'captures'
            output = Path(directory)/'output'
            captures.mkdir()
            output.mkdir()
            filename = '0000-message.xml'
            (output/filename).write_bytes(b'previous fixture')
            (output/'manifest.json').write_text(json.dumps([{'file': filename}]))
            result = subprocess.run([sys.executable, str(SCRIPT), str(captures), str(output)], capture_output=True, check=False)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual((output/filename).read_bytes(), b'previous fixture')

    def test_empty_recordings_preserve_previous_outputs(self):
        for recording in ('', '<message><body>incomplete', '<stream:stream xmlns:stream="http://etherx.jabber.org/streams">'):
            with self.subTest(recording=recording), tempfile.TemporaryDirectory() as directory:
                captures = Path(directory)/'captures'
                output = Path(directory)/'output'
                captures.mkdir()
                output.mkdir()
                (captures/'empty.xml').write_text(recording)
                filename = '0000-message.xml'
                (output/filename).write_bytes(b'previous fixture')
                manifest = json.dumps([{'file': filename}]).encode()
                (output/'manifest.json').write_bytes(manifest)
                result = subprocess.run([sys.executable, str(SCRIPT), str(captures), str(output)], capture_output=True, check=False)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual((output/filename).read_bytes(), b'previous fixture')
                self.assertEqual((output/'manifest.json').read_bytes(), manifest)

    def test_cr_only_http_ciphertext(self):
        cipher = base64.b64encode(bytes(range(16))).decode()
        for separator in ('\n', '\r\n', '&#13;', '&#xD;'):
            for header in ('PUT /x HTTP/1.1', 'HTTP/1.0 200 OK'):
                with self.subTest(separator=separator, header=header):
                    raw = f'<message><body>{header}{separator}Content-Length: 24{separator}{separator}{cipher}</body></message>'
                    cleaned, _ = corpus.sanitize(raw.encode())
                    text = ET.fromstring(cleaned).find('{jabber:client}body').text
                    payload = text.replace('\r\n', '\n').replace('\r', '\n').split('\n\n', 1)[1]
                    self.assertEqual(base64.b64decode(payload, validate=True), bytes(16))


if __name__ == '__main__':
    unittest.main()
