"""The relay's injected GET must land between relayed stanzas."""
import importlib.util
import io
from pathlib import Path
import socket
import stat
import sys
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location('xmpp_relay', Path(__file__).with_name('xmpp-relay.py'))
relay = importlib.util.module_from_spec(spec)
spec.loader.exec_module(relay)


class StanzaBoundaryTests(unittest.TestCase):
    header = b"<stream:stream xmlns='jabber:client' xmlns:stream='http://etherx.jabber.org/streams'>"
    declaration = b"<?xml version='1.0' encoding='UTF-8'?>"

    def test_fragmented_declarations_at_every_split(self):
        for restarted in (False, True):
            for split in range(1, len(self.declaration)):
                with self.subTest(restarted=restarted, split=split):
                    b = relay.StanzaBoundary()
                    if restarted:
                        b.feed(self.header + b'<success/>')
                    b.feed(self.declaration[:split])
                    self.assertTrue(b.ok)
                    self.assertFalse(b.between())
                    b.feed(self.declaration[split:] + self.header + b'<iq/>')
                    self.assertTrue(b.ok)
                    self.assertTrue(b.between())
                    b.feed(b'<message><body>')
                    self.assertFalse(b.between())
                    b.feed(b'x</body></message>')
                    self.assertTrue(b.between())

    def test_bytewise_streams_with_optional_declarations(self):
        b = relay.StanzaBoundary()
        for declaration in (self.declaration, self.declaration, b'', self.declaration):
            stream = declaration + self.header + b'<iq/>'
            for value in stream:
                b.feed(bytes([value]))
                self.assertTrue(b.ok)
            self.assertTrue(b.between())
            self.assertFalse(b.pending)

    def test_declaration_buffer_is_bounded(self):
        for complete in (False, True):
            with self.subTest(complete=complete):
                b = relay.StanzaBoundary()
                b.feed(self.header)
                data = b"<?xml version='1.0'" + b' ' * b.max_declaration_bytes
                if complete:
                    b.feed(data + b'?>')
                else:
                    for value in data:
                        b.feed(bytes([value]))
                        self.assertLessEqual(len(b.pending), b.max_declaration_bytes)
                self.assertLessEqual(len(b.pending), b.max_declaration_bytes)
                self.assertFalse(b.ok)
                self.assertFalse(b.between())
                b.feed(self.header + b'<iq/>')
                self.assertFalse(b.between())

    def test_declaration_like_text_is_not_stripped(self):
        b = relay.StanzaBoundary()
        parsed = []
        b.parser.CharacterDataHandler = parsed.append
        data = self.header + b'<message><body><![CDATA[' + self.declaration + b']]></body></message>'
        for value in data:
            b.feed(bytes([value]))
            self.assertTrue(b.ok)
        self.assertTrue(b.between())
        self.assertEqual(''.join(parsed), self.declaration.decode())

    def test_tracks_stanzas_across_reads_and_restarts(self):
        b = relay.StanzaBoundary()
        header = b"<?xml version='1.0'?><stream:stream xmlns='jabber:client' xmlns:stream='http://etherx.jabber.org/streams'>"
        for chunk, between in [
            (header, True),
            # A start tag cut by a read, completed by a shorter one.
            (b'<message to="rrccontact_123456789@wa2-mz36-qrmzh6.bosch.de/app"', False),
            (b'>', False),
            (b'</message>', True),
            (b'<message><body>', False),
            (b'GET /a HTTP/1.1\r\n', False),
            (b'</body></message>', True),
            # The restart after SASL opens a fresh stream, declaration and all.
            (header, True),
            (b'<iq type="result" id="b"', False),
            (b'/>', True),
            # A literal '>' inside an attribute does not end the tag.
            (b'<message note="a>', False),
            (b'b"/>', True),
            (b'<!-- a>', False),
            (b' -->', True),
        ]:
            b.feed(chunk)
            self.assertEqual(b.between(), between, chunk)

    def test_unparseable_stream_never_injects(self):
        b = relay.StanzaBoundary()
        b.feed(b'<stream:stream>')
        b.feed(b'<<')
        self.assertFalse(b.between())


class StatusGetTests(unittest.TestCase):
    header = b'<stream:stream from="rrcgateway_123456789" to="wa2-mz36-qrmzh6.bosch.de" version="1.0">'

    def test_addressed_from_the_device_header(self):
        self.assertEqual(relay.status_get(b'<?xml?>' + self.header), (
            b'<message to="rrcgateway_123456789@wa2-mz36-qrmzh6.bosch.de" type="chat" '
            b'from="rrccontact_123456789@wa2-mz36-qrmzh6.bosch.de/localprobe">'
            b'<body>GET /ecus/rrc/uiStatus HTTP/1.1\r\nUser-Agent: NefitEasy\r\n\r\n</body></message>'))

    def test_unexpected_header_values_inject_nothing(self):
        # Header bytes become XML, so only plain JID characters pass.
        for header in [
            self.header.replace(b'bosch.de"', b'bosch.de&amp;x"'),
            self.header.replace(b'bosch.de"', b'bosch.de<x"'),
            self.header.replace(b'123456789"', b'12345678x"'),
            self.header.replace(b'to=', b'xto='),
        ]:
            self.assertIsNone(relay.status_get(header), header)

    def test_saves_only_the_probe_reply(self):
        app = b'<message to="rrccontact_123456789@host/app"><body>HTTP/1.0 200 OK</body></message>'
        probe = b'<message to="rrccontact_123456789@host/localprobe"><body>HTTP/1.0 200 OK</body></message>'
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(relay, 'directory', Path(directory)):
            self.assertTrue(relay.save_response(app + probe, 'x'))
            self.assertEqual((Path(directory) / 'x-local-status-response.xml').read_bytes(), probe)
            self.assertFalse(relay.save_response(app, 'y'))

    def test_old_expat_refused_at_startup(self):
        argv = ['xmpp-relay.py', '--listen', '127.0.0.1:0', '--device-ip', '192.0.2.1', '--status-get']
        stderr = io.StringIO()
        # Past the guard, main would create directories and listen forever.
        with mock.patch.object(relay, 'expat', SimpleNamespace(XMLParserType=object)), \
                mock.patch.object(sys, 'argv', argv), mock.patch('sys.stderr', stderr), \
                mock.patch.object(relay.private_output, 'directory', side_effect=AssertionError('guard passed')), \
                self.assertRaises(SystemExit) as exit:
            relay.main()
        self.assertEqual(exit.exception.code, 2)
        self.assertIn('reparse-deferral', stderr.getvalue())


class RelayTests(unittest.TestCase):
    def test_probe_waits_for_stanza_boundary(self):
        def until(sock, marker):
            data = b''
            while marker not in data:
                chunk = sock.recv(65536)
                if not chunk:
                    break
                data += chunk
            return data

        with tempfile.TemporaryDirectory() as directory, socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            listener.listen(1)
            device, relayed = socket.socketpair()
            args = SimpleNamespace(upstream=f'127.0.0.1:{listener.getsockname()[1]}', status_get=True)
            with mock.patch.object(relay, 'args', args), mock.patch.object(relay, 'directory', Path(directory)), \
                    mock.patch('builtins.print'):
                worker = threading.Thread(target=relay.relay, args=(relayed, ('127.0.0.1', 0)))
                worker.start()
                bosch, _ = listener.accept()
                device.settimeout(5)
                bosch.settimeout(5)
                device.sendall(StatusGetTests.header)
                until(bosch, b'>')
                # Between stanzas, but before the device has answered HTTP:
                # its session may not be ready for the probe yet.
                bosch.sendall(b"<stream:stream xmlns='jabber:client' xmlns:stream='http://etherx.jabber.org/streams'><stream:features/>")
                early = until(device, b'<stream:features/>')
                bosch.sendall(b'<iq type="result" id="a"/>')
                self.assertNotIn(b'localprobe', early + until(device, b'id="a"/>'))
                # Waiting for each forwarded fragment prevents TCP from
                # coalescing the declaration into one relay read.
                restart = StanzaBoundaryTests.declaration + StanzaBoundaryTests.header
                for chunk in (restart[:2], restart[2:14], restart[14:]):
                    bosch.sendall(chunk)
                    self.assertEqual(until(device, chunk), chunk)
                # Bosch is mid-stanza when the device's first reply arrives.
                bosch.sendall(b'<message><body>')
                device.sendall(b'<message><body>HTTP/1.0 200 OK</body></message>')
                device.settimeout(0.5)
                self.assertNotIn(b'localprobe', until(device, b'<body>'))
                device.settimeout(5)
                bosch.sendall(b'x</body></message>')
                self.assertIn(b'uiStatus', until(device, b'</message><message'))
                device.sendall(b'<message to="rrccontact_123456789@host/localprobe"><body>HTTP/1.0 200 OK</body></message>')
                device.close()
                upstream = until(bosch, b'never')
                bosch.close()
                worker.join(5)
            self.assertNotIn(b'uiStatus', upstream)
            saved = list(Path(directory).glob('*-local-status-response.xml'))
            self.assertEqual(len(saved), 1)
            self.assertEqual(stat.S_IMODE(saved[0].stat().st_mode), 0o600)
            recorded = list(Path(directory).glob('*-cloud.bin'))
            self.assertEqual(len(recorded), 1)
            self.assertIn(restart, recorded[0].read_bytes())


if __name__ == '__main__':
    unittest.main()
