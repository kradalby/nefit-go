#!/usr/bin/env python3
"""Capture only the target's IPv4 Ethernet frames into a private pcap file."""
import argparse
import os
import signal
import socket
import struct
import time

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("output")
parser.add_argument("--interface", default="enp5s0")
parser.add_argument("--target", default="192.168.156.96")
args = parser.parse_args()
os.umask(0o077)
target = socket.inet_aton(args.target)
running = True


def stop(*_):
    global running
    running = False


signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
with socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(3)) as sock:
    sock.bind((args.interface, 0))
    sock.settimeout(1)
    with open(args.output, "xb", buffering=0) as output:
        output.write(struct.pack("<IHHIIII", 0xA1B2C3D4, 2, 4, 0, 0, 65535, 1))
        print(f"Capturing {args.target} on {args.interface} to {args.output}", flush=True)
        while running:
            try:
                packet = sock.recv(65535)
            except socket.timeout:
                continue
            if len(packet) < 34 or packet[12:14] != b"\x08\x00":
                continue
            if target not in (packet[26:30], packet[30:34]):
                continue
            now = time.time_ns()
            output.write(struct.pack("<IIII", now // 10**9, (now % 10**9) // 1000,
                                     len(packet), len(packet)))
            output.write(packet)
