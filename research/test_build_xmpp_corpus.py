"""Credential redaction must survive every supported SASL spelling."""
import base64
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
import xml.etree.ElementTree as ET

SCRIPT = Path(__file__).with_name('build-xmpp-corpus.py')
spec = importlib.util.spec_from_file_location('corpus', SCRIPT)
corpus = importlib.util.module_from_spec(spec)
spec.loader.exec_module(corpus)


class SanitizerTests(unittest.TestCase):
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


if __name__ == '__main__':
    unittest.main()
