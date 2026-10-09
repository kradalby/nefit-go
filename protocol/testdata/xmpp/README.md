This corpus comes from real device/cloud and experimental offline traffic; the recording names in `manifest.json` give each stanza's source. It is sanitized test data, not a replayable authentication session.

`manifest.json` indexes each distinct complete stanza, its SHA-256, independent namespace/body/status expectations, and every occurrence's recording name, original byte offset and length. `streams.json` indexes each distinct open stream header with the recording and offset of its first occurrence only: repeated headers, including restarts after authentication, collapse into one entry, and no lengths are recorded. Receive boundaries are not XML stanza boundaries.

Private originals stay outside the repository with restricted permissions; by convention their checksums are kept beside them in `originals.json`, which no tool here writes or reads. Transparent recordings contain both directional byte streams plus `events.jsonl` with direction, offset, receive length and UTC Unix nanoseconds. Because the device never negotiates STARTTLS, recordings hold the whole plaintext session, including SASL proofs; do not commit them.

## Provenance

Each `manifest.json` occurrence names its recording. The path identifies who produced the bytes:

| Recording | Producer |
| --- | --- |
| `<n>-device.bin`, `<n>-cloud.bin` | `research/xmpp-relay.py` between the device and Bosch; device recordings may include a reply to its optional local status GET |
| `offline-<n>-device.bin`, `offline-status-response.xml` | `research/offline-status-probe.py`; device bytes only. Current runs name the response `offline-<n>-status-response.xml` |
| `offline-device.bin` | an earlier, unnumbered offline server run; device bytes only |
| `local-status-response.xml` | device reply saved by `research/xmpp-relay.py`. Current runs name it `<n>-local-status-response.xml` |
| `completed-local-status-request.xml` | single saved stanza; its producer is not in this repository |
| `offline/<n>/device-to-server.xmpp` | `research/record-xmpp.py` in front of `nefit serve --mode offline`; device bytes |
| `offline/<n>/server-to-device.xmpp` | the same recorder; **nefit-go output** |
| `both/<n>/device-to-server.xmpp` | `research/record-xmpp.py` in front of `nefit serve --mode both`; device bytes |
| `both/<n>/server-to-device.xmpp` | the same recorder; **nefit-go output**, including Bosch stanzas re-encoded by nefit-go |
| `cloud-followup/<n>/*.xmpp` | `research/record-xmpp.py` follow-up sessions; the upstream is recorded in each session's private `metadata.json` |

A fixture whose occurrences are all nefit-go output shows what this implementation sends, not what the firmware or Bosch require. Use stock-traffic occurrences as protocol evidence. `streams.json` names the same recordings for stream headers, but only for each header's first occurrence.

Transformations:

- Gateway/contact serials, UUIDs, service resource timestamps and stream header IDs are synthetic. Resources the server assigned, which may reuse a stream ID, are kept.
- PLAIN authorization identities, login names and passwords are replaced with synthetic values in initial and continuation payloads. DIGEST identities, realms, nonces, digest URIs and proofs are rewritten inside decoded base64, including whitespace and CDATA representations. These proofs are intentionally invalid and cannot validate a real DIGEST-MD5 exchange.
- Body payloads that are strict, block-aligned base64, whether bare or after HTTP headers, in text, CDATA, character references or wrapped lines, are replaced with base64 zero bytes of the same decoded length. Comments inside ciphertext bodies are replaced; the Go reader rejects any comment left in a fixture. HTTP headers and ciphertext lengths remain intact. Any other body content is kept only if the audit below accepts it. These are opaque XML/HTTP fixtures, not AES or decrypted JSON fixtures.
- SASL failure text, stanza error text, `jabber:iq:version` name/os, and `jabber:iq:auth` password/digest and `jabber:iq:register` password become `redacted`.
- Stanzas without namespace declarations get `jabber:client` (message, iq, presence) or the streams namespace (`stream:features`). The generator hardcodes these to match the captured stream headers; it does not read each stanza's original stream. Quote style, HTTP line endings and the remaining captured XML structure are retained.
- `.open` files are deliberately incomplete XML documents: XMPP streams remain open across stanzas and restart after authentication.

After redaction, an audit rejects the build if any text, attribute value, namespace or comment still holds a base64 or hex run of 16 or more characters that is not zero bytes, a synthetic identity, an entity-caps hash or a camelCase identifier such as a resource path or MIME type. Decoded SASL payloads, and body payloads with whitespace and non-ASCII characters removed, are checked the same way, and JID localparts may not contain digits other than the synthetic serial. The audit is a heuristic backstop: short secrets and passwords in prose can still pass, so review fixtures before publishing.

Coverage includes stream features, SASL authentication, binding/session IQs, version IQ queries, presence, HTTP GET/PUT requests and responses (including write acknowledgements), and opaque time/weather service messages. Firmware-version and update-strategy reads are present; actual firmware updates, TLS, cold boot and decoded time/weather payloads are not covered.

## Test coverage

- `protocol/corpus_test.go` checks fixture hashes, namespace expectations, `MessageStanza` bodies, HTTP status extraction and SASL base64. The set of `*.xml` files must equal the manifest's plain generator-style file names, and every body payload must be absent or base64 zero bytes, so `go test` alone catches a ciphertext leak.
- `internal/xmpp/xml_test.go` reads each fixture through `xmpp.Reader` in 1/7/4096-byte chunks and checks typed decoding, generic XML, and `xmpp.Marshal` in both its cloud and device forms, as the transports use it. Cloud text remains exact; literal device HTTP line endings normalize to LF when decoded. Each fixture with its last byte cut must read as truncated, not complete.
- `internal/xmpp/xml_test.go` also checks that `streams.json` covers every `.open` file with plain names and provenance, and round-trips each header. Restarts are synthetic: each captured header is read twice through one decoder to exercise buffered restart handling.
- `client/local_test.go` (`TestDeviceOutputMatchesSuccessfulCaptures`) compares the device server's stream features, bind and session results, and a GET request byte for byte with captures the firmware accepted.
- `research/test_build_xmpp_corpus.py` requires every committed `.xml` file to be a fixed point of the sanitizer, audit included, and every `.open` file to pass identity redaction and the audit unchanged. It checks every committed body payload is zeroed and that alternate spellings, unaligned or non-standard base64, hex, blobs outside bodies, free text and bare-serial JIDs are redacted or fail the build.

Comparisons are semantic, not byte identity: `encoding/xml` normalizes literal CRLF and may change prefixes, quote styles and empty-element spelling. Typed comparisons ignore attribute order, insignificant whitespace between element-only children and the order of advertised stream features. Body text and extension contents remain exact.

Not covered by fixtures:

- Apart from the device login and GET pinned above, encoder output is compared by XML semantics, not captured byte identity.
- No test replays a complete captured handshake through the device server.
- Firmware updates, TLS, cold boot and decoded time/weather payloads.

To record (substitute the actual backend listener):

    python3 research/record-xmpp.py --listen <listen-ip>:5222 \
      --upstream <nefit-serve-ip>:<port> --device-ip <device-ip> \
      --output <capture-dir>/offline

`research/xmpp-relay.py` and `research/offline-status-probe.py` write to `$XDG_STATE_HOME/nefit-go/research` (or `~/.local/state/nefit-go/research`) unless `--output` is given. Their output directories, and those of `research/record-xmpp.py`, must be owned by the running user with mode 0700, and existing files are never overwritten or followed. `research/capture-device.py` only creates new files under umask 077.

To regenerate from private originals:

    python3 research/build-xmpp-corpus.py <capture-dir> protocol/testdata/xmpp

To rebuild only the existing recording set, add `--recordings-from <previous-manifest.json>` (`-` reads the manifest from stdin). Missing or empty capture input fails before changing generated files.

The generator fails on invalid XML, invalid SASL base64, unsupported SASL mechanisms/payloads and anything the audit flags, rather than silently including unsanitized data; existing output is untouched on failure. Additional protocol fields may require additional redaction rules. `python3 -m unittest discover -s research -p 'test_*.py'` runs the sanitizer, private-output and relay tests; CI runs the same tests. Extraction is limited to the stanza types listed above and deliberately excludes incomplete stanzas.
