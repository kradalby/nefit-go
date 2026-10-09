"""Research output must stay private to the recording user."""
import os
from pathlib import Path
import stat
import tempfile
import unittest
from unittest import mock

import private_output


class PrivateOutputTests(unittest.TestCase):
    def test_creates_private_directory_and_files(self):
        with tempfile.TemporaryDirectory() as root:
            directory = private_output.directory(Path(root) / 'state' / 'research')
            self.assertEqual(stat.S_IMODE(directory.stat().st_mode), 0o700)
            with private_output.create(directory / 'capture.bin') as capture:
                capture.write(b'private')
            self.assertEqual(stat.S_IMODE((directory / 'capture.bin').stat().st_mode), 0o600)
            self.assertEqual(private_output.directory(directory), directory)

    def test_short_writes_are_finished(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / 'capture.bin'
            real, calls = os.write, []

            # Each underlying write takes one byte, as a full pipe or disk might.
            def short(fd, data):
                calls.append(len(data))
                return real(fd, bytes(data[:1]))
            with private_output.create(path) as capture, mock.patch.object(private_output.os, 'write', short):
                self.assertEqual(capture.write(b'private'), 7)
            self.assertEqual(path.read_bytes(), b'private')
            self.assertEqual(calls, [7, 6, 5, 4, 3, 2, 1])

    def test_existing_paths_are_never_reused(self):
        with tempfile.TemporaryDirectory() as root:
            directory = private_output.directory(Path(root) / 'research')
            (directory / 'planted').write_bytes(b'planted')
            (directory / 'link').symlink_to(Path(root) / 'elsewhere')
            for name in ('planted', 'link'):
                with self.subTest(name=name), self.assertRaises(FileExistsError):
                    private_output.create(directory / name)
            self.assertFalse((Path(root) / 'elsewhere').exists())

    def test_shared_or_linked_directories_are_rejected(self):
        with tempfile.TemporaryDirectory() as root:
            shared = Path(root) / 'shared'
            shared.mkdir(mode=0o755)
            os.chmod(shared, 0o755)
            private = Path(root) / 'private'
            private.mkdir(mode=0o700)
            # A link is refused even to a private directory: it could be swapped.
            (Path(root) / 'link').symlink_to(private)
            for path in (shared, Path(root) / 'link'):
                with self.subTest(path=path.name), self.assertRaises(SystemExit):
                    private_output.directory(path)

    def test_directories_of_other_users_are_rejected(self):
        other = os.geteuid() + 1
        with tempfile.TemporaryDirectory() as root, \
                mock.patch.object(private_output.os, 'geteuid', lambda: other):
            with self.assertRaises(SystemExit):
                private_output.directory(Path(root))


if __name__ == '__main__':
    unittest.main()
