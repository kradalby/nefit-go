This corpus comes from real device/cloud and experimental offline traffic captured on 2026-10-05 (thermostat firmware 02.22.00). It is sanitized test data, not a replayable authentication session.

`manifest.json` indexes each distinct complete stanza, its SHA-256, independent namespace/body/status expectations, and every occurrence's recording name and original byte offset. `streams.json` indexes open stream headers, including authentication restarts. Receive boundaries are not XML stanza boundaries.

Private originals are retained outside the repository at `~/.local/state/nefit-go/captures/2026-10-05`, with restricted permissions and original checksums in `originals.json`. New transparent recordings contain both directional byte streams plus `events.jsonl` recording direction, offsets, receive lengths and UTC Unix nanoseconds. They contain sensitive authentication and device data; do not commit them.

Transformations:

- Gateway/contact serials, UUIDs, service resource timestamps and stream IDs are synthetic.
- PLAIN authorization identities, login names and passwords are replaced with synthetic values in initial and continuation payloads. DIGEST identities, realms, nonces, digest URIs and proofs are rewritten inside decoded base64, including whitespace and CDATA representations. These proofs are intentionally invalid and cannot validate a real DIGEST-MD5 exchange.
- Encrypted body blobs are replaced with base64-encoded zero bytes of the same decoded length. HTTP headers and ciphertext lengths remain intact. These are opaque XML/HTTP fixtures, not AES or decrypted JSON fixtures.
- Standalone stanzas receive the namespace declarations inherited from their original stream. Quote style, HTTP line endings and the remaining captured XML structure are retained.
- `.open` files are deliberately incomplete XML documents: XMPP streams remain open across stanzas and restart after authentication.

Coverage includes stream features, SASL authentication, binding/session IQs, version IQ queries, presence, HTTP GET/PUT requests and responses (including write acknowledgements), and opaque time/weather service messages. Firmware-version and update-strategy reads are present; actual firmware updates, TLS, cold boot and decoded time/weather payloads are not covered.

`xmpp/xml_test.go` additionally validates complete typed stanzas against the original captured semantics, unknown extensions, stream restarts, malformed input, and firmware lexical adapters. `protocol/corpus_test.go` verifies fixture integrity, namespace resolution, reads split into 1/7/4096-byte chunks, XML token semantic round trips, legacy MessageStanza known-field round trips, HTTP status extraction, SASL base64 decoding and truncated-stanza rejection. XML equivalence is semantic, not byte identity: encoding/xml normalizes literal CRLF and may change prefixes, quote styles and empty-element spelling. Typed comparisons ignore attribute order, insignificant whitespace between element-only children and the ordering of advertised stream features; body text and extension contents remain exact. The shared typed codec is used by cloud, offline and both transports. The corpus does not establish full offline time/weather or update correctness.

To record (substitute the actual backend listener):

    python3 research/record-xmpp.py --listen 10.65.0.27:5222 \
      --upstream 127.0.0.1:5224 --device-ip 192.168.156.96 \
      --output "$HOME/.local/state/nefit-go/captures/2026-10-05/offline"

To regenerate from private originals:

    python3 research/build-xmpp-corpus.py \
      "$HOME/.local/state/nefit-go/captures/2026-10-05" protocol/testdata/xmpp

The generator fails on invalid XML, invalid SASL base64 or unsupported SASL mechanisms/payloads rather than silently including unsanitized data. Review fixtures before publishing; additional protocol fields may require additional redaction rules. `python3 -m unittest discover -s research -p 'test_*.py'` checks credential redaction and preservation of existing output after failed validation; CI runs the same tests. Extraction is limited to the stanza types listed above and deliberately excludes incomplete stanzas.
