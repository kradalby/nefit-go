# Nefit Easy Go

A Go library for controlling Nefit Easy smart thermostats via XMPP, converted from the JavaScript [nefit-easy-core](https://github.com/robertklep/nefit-easy-core) implementation.

## Supported Devices

As per the library this is ported from, it should work with the following, however, only Nefit Easy is tested.

- Nefit Easy (Netherlands)
- Worcester Wave (UK)
- Junkers Control (Germany)
- Buderus Logamatic TC100 (Germany)
- Bosch Greenstar CT100 (Various regions)

## Installation

```bash
go get github.com/kradalby/nefit-go
```

## CLI Usage

The `nefit` CLI tool provides easy access to all library functions:

### Configuration

Use command-line flags or environment variables:

```bash
# Using flags
nefit --serial <serial> --access-key <key> --password <password> status

# Using environment variables (recommended)
export NEFIT_SERIAL_NUMBER=<serial>
export NEFIT_ACCESS_KEY=<key>
export NEFIT_PASSWORD=<password>
nefit status
```

The cloud transport connects directly to Bosch and requires verified STARTTLS before sending credentials. It does not use HTTP proxy environment variables or follow XMPP host redirects.

### Commands

```bash
# Get system status
nefit status
nefit --pretty status         # Pretty-print JSON

# Get system pressure
nefit pressure

# Get/set hot water
nefit hot-water
nefit hot-water on
nefit hot-water off

# Set temperature (manual setpoint and active-program override)
nefit set temperature 21.5

# Set user mode
nefit set user-mode manual
nefit set user-mode clock

# Raw GET/PUT requests
nefit get /ecus/rrc/uiStatus
nefit put /heatingCircuits/hc1/temperatureRoomManual '{"value":21.5}'

# Print backend push notifications (debug)
nefit subscribe

# Run the device server and HTTP API (see Device server modes).
# The XMPP listener defaults to loopback; bind an address the thermostat reaches.
nefit serve --device-ip 192.0.2.10 --xmpp-listen 192.0.2.1:5222

# Version
nefit version

# Help
nefit --help
nefit set --help
```

Without a subcommand, or with an unknown one, `nefit` prints usage and exits 2; invalid flags also exit 2. Configuration `serve` rejects at startup, missing credentials included, and a non-positive `--timeout` for any command exit 78 (`EX_CONFIG`). Other failures exit 1, including a listener address that is in use or not assigned yet, since a restart can fix those.

## Features

### Device server modes

`nefit serve` and the embeddable `server.Server` let the thermostat log in to nefit instead of Bosch, and expose the same client API over that connection. `offline` serves the device locally. `both` also relays its login and traffic to Bosch, so the official app keeps working. The device answers one request at a time: local and app requests share one queue, local requests go ahead of queued app requests, and replies are routed by the address they are sent to.

```sh
nefit --timeout 15s serve --mode both \
  --device-ip 192.0.2.10 --xmpp-listen 192.0.2.1:5222 \
  --http-listen 127.0.0.1:8088 --updates block
```

The root `--timeout` (default 30s) bounds each device request attempt, each HTTP API request, and how long the device may take to answer before its request counts as unanswered. Without a device, `serve` keeps listening; API requests wait up to `--timeout` for it to log in.

Request lifecycle:

- Push notifications continue during and between requests. A notification received before a request is sent cannot answer it, even when its XML arrives in fragments or remains buffered.
- A request that expires before reaching the device costs nothing and is retried while time remains.
- A caller that gives up after its request reached the device does not drop the session. The request keeps the device until its reply arrives, which is then discarded, or until `--timeout` passes. A request still unanswered then reconnects the device, Bosch relay included, so a late reply cannot answer the next request.
- Writes that reached the device are never replayed.
- When a newly connecting device completes its login, it replaces a stale (half-open) session at once. A failed login leaves the current session alone.

In `both`:

- If Bosch is unreachable, rejects the relayed login (its bind or session included), or drops the relay, the device is served offline. Relaying is retried after 1 minute, doubling on each consecutive failure up to 32 minutes; a relay that stays up for a minute resets the backoff. Bosch ending the old relay because a reconnecting device logged in again is not counted.
- After a loss, nefit checks that Bosch offers an XMPP login again, then reconnects the device once no request is in flight. That check cannot prove Bosch will accept the gateway.
- If Bosch disconnects after login, local control continues; app requests not yet sent are dropped.
- App requests that arrive during login wait until it completes.
- An app request the device does not answer in time gets 504. That app session's later requests also get 504 until the old reply arrives or the device reconnects. Other app sessions and local control continue.
- Malformed, ambiguous or oversized app requests get 400, or 403 when `--updates block` refuses them. With 32 app requests already waiting, the next gets 503; local requests are not limited, as the client sends one at a time.
- Messages without a request line and error stanzas are never answered: Bosch services answer every message, so a reply could loop. An error quoting a request is dropped, so it cannot reach the device as a request.
- A stanza from Bosch over the 64 KiB reader limit (the stream cannot resume after it), or 64 app sessions each waiting on an unanswered request, end the relay like a Bosch drop.
- The relayed gateway session to Bosch stays plaintext, as the device sends it. `both` cannot relay a device that negotiates STARTTLS.

`--updates block` refuses firmware-update traffic on XMPP:

- writes under `/gateway/update`, in any letter case;
- any malformed request line, GET included, and any write whose target needs decoding or normalising to read (`%`, `#`, `.`/`..` segments, `//`, `;`, `\`, absolute URIs);
- message bodies containing markup; the rules above also apply to request lines in the text of any element, not only a body, whether or not markup splits them;
- traffic to or from `gservice_update` and `gservice_firmware`, the services named by `--update-services` (which requires `--updates block`), and any JID that is not plain ASCII, since servers may fold such spellings into these.

The policy applies in both directions, during login and after. Refused app requests get 403; local API writes never reach the device and return 403 once one is connected; before that they wait for its login like any request. Reads such as the firmware version and update strategy stay available. This is best effort: independent downloads and update mechanisms not seen in captures need network egress rules. No real firmware update has been captured, so a complete freeze is not established.

Bosch time and weather services are relayed in `both` and left unanswered offline; their payload encoding is not understood. Device control and app coexistence were tested on firmware 02.22.00. Cold boot without internet and multi-day behaviour still need hardware testing.

#### HTTP API

| Path | Methods | Result/input |
| --- | --- | --- |
| `/healthz` | GET | device and upstream connection state; 503 if device disconnected |
| `/api/status` | GET | status; optional `?outdoor=true` |
| `/api/pressure` | GET | boiler pressure |
| `/api/hot-water` | GET, PUT | current state; PUT `{"value":true}` |
| `/api/temperature` | PUT | `{"value":14.5}` |
| `/api/user-mode` | PUT | `{"value":"manual"}` or `{"value":"clock"}` |
| `/<thermostat-path>` | GET, PUT | decrypted device JSON; PUT JSON value |
| `/bridge/<thermostat-path>` | GET, PUT, POST | compatibility alias for raw paths; POST is sent as PUT |

- HEAD on raw and `/bridge` paths forwards a GET. Unknown `/api` routes return 404; wrong methods on known API routes and `/healthz` return 405.
- Writes require `Content-Type: application/json` (else 415) and at most 64 KiB (else 413). Raw writes send the JSON compacted but otherwise unchanged; raw reads return device numbers as float64.
- Errors are JSON `{"error": "..."}`, except routing 404/405:
  - 400: invalid input, path or value (temperature outside 5–30 °C, unknown user mode);
  - 403: untrusted Host, cross-site request, or a write refused by `--updates block`;
  - 408: request body too slow;
  - 502: device error;
  - 504: device timeout.
- HTTP has no authentication and binds to loopback by default.
- Only `localhost`, loopback IPs and the local IP the client connected to are trusted as Host, which blocks DNS rebinding. Add trusted LAN or reverse-proxy names with `--http-allowed-hosts nefit.example,proxy.example:8443`. An invalid entry stops startup. Forwarded headers grant nothing, and `0.0.0.0`/`[::]` are never trusted.
- Browser requests marked cross-site are rejected for every method, and cross-origin writes are rejected. Browsers mark requests only to HTTPS or `localhost` URLs, so a page elsewhere can still trigger, though not read, a GET through a plain-HTTP LAN or proxy name. Put remote access behind an authenticating HTTPS proxy.
- Rejections answer without reading an unread body; fully read requests keep the connection alive.
- Interrupt/SIGTERM drains HTTP for up to `--timeout` plus 5s before closing XMPP. A second signal exits immediately.

Listener errors are retried with backoff. Only a closed or invalid listener ends `Run` with `client.ErrListenerFailed`; the CLI then drains HTTP and exits with an error so its supervisor can restart it.

#### Embedding

```go
s, err := server.New(server.Config{
    Device: client.Config{SerialNumber: serial, AccessKey: accessKey, Password: password},
    LocalOptions: client.LocalOptions{
        Mode:          client.ModeBoth,
        ListenAddress: "192.0.2.1:5222",
        DeviceIP:      netip.MustParseAddr("192.0.2.10"),
        UpdatePolicy:  client.UpdatesBlock,
    },
})
if err != nil { return err }
defer s.Close()
// Devices are admitted until Close; Run reports a failed listener.
// All client commands and subscriptions are exposed.
return s.Run(ctx)
```

`New` binds the XMPP listener and, when configured, the DNS endpoint, and admits device logins until `Close` releases them and the session; no reconnect loop is needed. Call `Close` and `SetLogger` on the server, not its embedded `Client`, so they reach the DNS endpoint too. `Run` returns when ctx ends, `client.ErrClosed` when the server or its client is closed, or `client.ErrListenerFailed` when the listener fails. `Run` closes the server when it returns; when draining HTTP on the same signal, give `Run` a context cancelled after the drain. `NewLocalClient` and `New` reject settings that would be ignored or break the login: a missing device IP or one no device can have (unspecified, multicast, broadcast), an upstream address outside `both` or without a usable port, update services without blocking or naming no JID, and a DNS address the device cannot reach the XMPP listener on (loopback included, unless the device is local).

`Device.RetryTimeout` (default 2s) bounds each caller's attempt, while `RequestTimeout` (default 15s) is how long the device may hold the slot answering; a reply after the caller gave up is discarded. The CLI sets both to `--timeout`. Device refusals return `*client.HTTPError` with the status code.

For HTTP, call `server.NewHandler(s.Client, timeout, allowedHosts...)`; it returns an error for an invalid host. Serve it from an `http.Server` with `ReadHeaderTimeout`, `ReadTimeout` and a small `MaxHeaderBytes` set (the CLI uses 16 KiB); headers are buffered before any check runs. The handler bounds device work, not how long a client takes to send its request.

#### Protocol handling and security

- **XML:** stanzas are typed, and the stream reader is namespace aware, including authentication restarts and unknown extensions. `encoding/xml` supplies structure and escaping.
- **Device output:** written with literal HTTP newlines and compact empty elements, as the firmware expects. The cloud transport (contact login) keeps standard XML character references; stanzas relayed to Bosch in `both` keep the device form.
- **Reader limits:**
  - 64 KiB per stanza;
  - 256 elements and attributes per stanza;
  - nesting depth 64;
  - 128 active namespace bindings.
- **Rejected input:** comments, DTDs, processing instructions inside stanzas, and names or namespace declarations a namespace-aware peer could not read back.
- **Fixtures:** captured fixtures exercise both encoders; see [the corpus notes](protocol/testdata/xmpp/README.md) for provenance and coverage.
- **Device admission:** the XMPP listener checks the source IP and the gateway identity. Offline DIGEST-MD5 does not verify the gateway password, so firewall the listener to the thermostat.
- **Transport security:** the device stream is plaintext XMPP, with encrypted API bodies on the tested firmware. The contact login to Bosch uses verified STARTTLS.

### Taking over the device connection

**DNS override (preferred):**
- Override the original Bosch hostname for the thermostat or its IoT resolver only, and keep `--domain` set to that hostname.
- If `both` uses the overridden resolver, set `--upstream` to a real Bosch IP:port. Otherwise nefit connects back to itself.
- A global override also affects the phone app.
- Device DNS caching, other bootstrap names and hardcoded IPs are untested; DNS takeover is not yet proven reliable.

An optional device-scoped UDP/TCP DNS endpoint answers for the Bosch hostname:

```sh
nefit serve --mode offline --device-ip 192.0.2.10 \
  --xmpp-listen 192.0.2.1:5222 \
  --dns-listen 192.0.2.1:53 --dns-address 192.0.2.1
```

- **Required flags:** `--dns-listen` takes a specific IP:port, not a wildcard, because a wildcard UDP socket may answer from the wrong address. It needs `--dns-address`, a non-loopback IP the device can reach, and `--xmpp-listen` must bind that address or all addresses. `--dns-listen` and `--dns-address` must share the device IP's family. The endpoint answers the device IP's queries for `--domain`, both taken from the XMPP configuration.
- **Setup:** point the device's resolver or DHCP DNS at the endpoint. It answers only the device and `server.DNSConfig.AllowedPeers`.
- **Answers:**
  - A, AAAA and `_xmpp-client._tcp` SRV for the hostname, with the actual XMPP port unless `server.DNSConfig.XMPPPort` overrides it.
  - Other names at or below the hostname get NODATA or NXDOMAIN with a synthetic SOA, and are never forwarded; non-IN classes are refused.
  - EDNS queries get an EDNS reply that echoes the DNSSEC OK bit. A UDP answer larger than the query allows (512 bytes, or its EDNS size) is truncated, so the device retries over TCP.
  - A missing address family returns no data, so an IPv6 record cannot bypass an IPv4 override.
- **Forwarding:** unrelated names are refused unless `--dns-forward 192.0.2.53:53` names a resolver IP:port. Forwarded queries keep their transport, so truncated UDP answers can be retried over TCP. At most 8 are in flight; beyond that they get SERVFAIL, so a slow resolver cannot crowd out the Bosch hostname's answers.
- **Not done:** the endpoint does not advertise DHCP or take over an existing resolver. Forwarded answers pass unchanged, so an alias the resolver returns into the Bosch hostname would carry the real address.

**Without DNS:** if DNS cannot be changed, redirect the thermostat's outbound TCP/5222 to the XMPP listener.
- On UniFi, use destination NAT on IoT ingress: source the device IP, destination any, TCP port 5222, translated to the local server IP. Changing only the destination IP match does not redirect the cloud connection.
- Allow the device to reach the listener across VLANs.
- A dedicated access point or router with its own DNS also works.
- ARP spoofing and rogue DHCP are not supported.

### NixOS

A NixOS module is available as `nixosModules.default`:

```nix
services.nefit-go = {
  enable = true;
  package = nefit-go.packages.${pkgs.stdenv.hostPlatform.system}.default;
  environmentFile = "/run/secrets/nefit-env";
  mode = "both";
  deviceIP = "192.0.2.10";
  xmppAddress = "192.0.2.1:5222";
  updates = "block";
  # Optional device-scoped DNS endpoint.
  dns = {
    enable = true;
    listen = "192.0.2.1:53";
    address = "192.0.2.1";
  };
};
```

- **Options:** `requestTimeout` (default `"30s"`) is passed as `--timeout`, and `httpAllowedHosts` configures trusted HTTP names.
- **Evaluation-time checks:** the module rejects an `environmentFile` inside the Nix store, `updateServices` without `updates = "block"`, `upstream` without `mode = "both"`, a wildcard `dns.listen`, `dns.forward` equal to `dns.listen`, and an `xmppAddress` that does not accept `dns.address`.
- **Service:** systemd allows `requestTimeout` plus 35s for a clean stop before SIGKILL. Exit code 78 (configuration error) is not restarted; crashes and listener failures are. The service gets `CAP_NET_BIND_SERVICE` only when a listener uses a port below 1024.
- **Firewall:** configure separately, so only the thermostat reaches the XMPP listener.

### High-Level API

The library provides convenient methods for common operations:

```go
// Get system status
status, err := client.Status(ctx, includeOutdoorTemp)

// Get system pressure
pressure, err := client.Pressure(ctx)

// Set temperature (5–30 °C; anything else is client.ErrInvalidValue)
err := client.SetTemperature(ctx, 21.5)

// Set user mode (manual or clock)
err := client.SetUserMode(ctx, "manual")

// Control hot water
err := client.SetHotWaterSupply(ctx, true)
active, err := client.HotWaterSupply(ctx)
```

### Low-Level API

For direct access to any endpoint:

```go
// Raw GET request
data, err := client.Get(ctx, "/ecus/rrc/uiStatus")

// Raw PUT request
err := client.Put(ctx, "/heatingCircuits/hc1/temperatureRoomManual", map[string]interface{}{
	"value": 21.5,
})
```

## Debugging

### Enable Debug Logging

The library uses Go's standard `log/slog` package. To see detailed request/response information:

```go
import "log/slog"
import "os"

// Create a logger with debug level
logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
}))

// Set it on the client
client.SetLogger(logger)
```

Debug logs include:
- Exact JSON payloads being sent (decrypted)
- Encrypted payload lengths
- HTTP status codes and responses
- Retry attempts with backoff timing
- Full error context

### Common Issues

**Problem: `client.ErrInvalidValue` (`invalid value: mode …`) from SetUserMode**

The API only accepts `"manual"` or `"clock"`. `SetUserMode` rejects anything else, `"off"` included, before sending; a raw PUT with another value gets HTTP 400 from the device.

To turn off heating, use:
```go
client.SetUserMode(ctx, "manual")
client.SetTemperature(ctx, 5.0) // Set minimum temperature
```

**See [API_NOTES.md](API_NOTES.md)** for comprehensive documentation on:
- Valid values for all endpoints
- Retry behavior and exponential backoff
- Troubleshooting common errors
- Production recommendations

## Disclaimer

This library is based on reverse-engineering the Nefit Easy communications protocol. It is **not** officially supported by Bosch, Nefit, or any related companies.

**Use at your own risk.** The authors assume no responsibility for:
- Damage to your heating system
- Incorrect temperature settings
- Loss of warranty
- Any other issues arising from use of this library

## Acknowledgments

This Go implementation is based on and inspired by the JavaScript libraries by [Robert Klep](https://github.com/robertklep):

- [nefit-easy-core](https://github.com/robertklep/nefit-easy-core) - Core protocol implementation and XMPP client
- [nefit-easy-commands](https://github.com/robertklep/nefit-easy-commands) - High-level command API
- [nefit-easy-cli](https://github.com/robertklep/nefit-easy-cli) - Command-line interface design

## License

MIT License - see LICENSE file for details.
