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

The cloud transport connects directly with verified STARTTLS. It does not use HTTP proxy environment variables or follow XMPP host redirects.

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

# Set temperature (switches to manual mode)
nefit set temperature 21.5

# Set user mode
nefit set user-mode manual
nefit set user-mode clock

# Raw GET/PUT requests
nefit get /ecus/rrc/uiStatus
nefit put /heatingCircuits/hc1/temperatureRoomManual '{"value":21.5}'

# Help
nefit --help
nefit set --help
```

## Features

### Device server modes

The CLI and embeddable `server.Server` expose the existing client API through a local XMPP connection. `offline` accepts the device locally; `both` relays gateway authentication and traffic to Bosch while serving local requests. Local and cloud HTTP requests share one scheduler, and response destinations isolate their acknowledgements. The official app works in `both` mode.

```sh
go run ./cmd/nefit --timeout 15s serve --mode both \
  --device-ip 192.168.156.96 --xmpp-listen 10.65.0.27:5222 \
  --http-listen 127.0.0.1:8088 --updates allow
```

Use `--mode offline` for local control without a Bosch control connection. In `both`, a failed upstream dial falls back to offline operation. If Bosch disconnects after login, local control remains available. Restoring cloud authentication requires a device reconnect: the server periodically checks reachability and retires the device session when Bosch is reachable again. In-progress requests may fail during that transition; writes are not automatically replayed.

`--updates block` rejects writes under `/gateway/update` and drops traffic for `gservice_update` and `gservice_firmware`. `--update-services` adds comma-separated service JIDs/localparts. Firmware-version and update-strategy reads remain available. This policy covers XMPP traffic; independent downloads and unobserved update mechanisms require network egress rules. No actual firmware update transaction has been captured, so a complete firmware freeze is not established.

The HTTP API includes:

| Path | Methods | Result/input |
| --- | --- | --- |
| `/healthz` | GET | device and upstream connection state; 503 if device disconnected |
| `/api/status` | GET | status; optional `?outdoor=true` |
| `/api/pressure` | GET | boiler pressure |
| `/api/hot-water` | GET, PUT | current state; PUT `{"value":true}` |
| `/api/temperature` | PUT | `{"value":14.5}` |
| `/api/user-mode` | PUT | `{"value":"manual"}` or `{"value":"clock"}` |
| `/<thermostat-path>` | GET, PUT | decrypted device JSON; PUT JSON value |
| `/bridge/<thermostat-path>` | GET, PUT, POST | compatibility alias for raw API |

Embed the same server without HTTP:

```go
s, err := server.New(server.Config{
    Device: client.Config{SerialNumber: serial, AccessKey: accessKey, Password: password},
    Mode: server.Both,
    ListenAddress: "0.0.0.0:5222",
    DeviceIP: net.ParseIP("192.168.156.96"),
    UpdatePolicy: server.BlockUpdates,
})
if err != nil { return err }
defer s.Close()
// Either maintain connectivity with s.Run(ctx), or use the application's
// existing Connect/Done loop. All client commands and subscriptions are exposed.
return s.Run(ctx)
```

`server.NewHandler(s.Client, timeout)` optionally exposes HTTP. New binds the XMPP listener; Close owns the listener and session. An optional `Service` handler receives typed offline messages and can implement additional device services. Bosch time/weather payloads use an unresolved separate encoding/key: these services are forwarded in `both`, but are not yet implemented offline. Cold boot without internet and multi-day behavior still need hardware validation. Device control reads and writes and app coexistence have been verified live on firmware 02.22.00.

XML uses typed stanzas and a namespace-aware stream reader, including authentication restarts and unknown extensions. `encoding/xml` supplies structure and escaping. Token-aware output adapters preserve the firmware's literal HTTP newlines, omit redundant namespace declarations within streams and compact empty elements. Captured fixtures test both XML semantics and these lexical requirements; see [the corpus notes](protocol/testdata/xmpp/README.md).

The XMPP listener restricts source IP and gateway identity. Offline DIGEST-MD5 compatibility does not verify the gateway password; use a device-scoped firewall rule. HTTP binds to loopback by default and has no authentication. The device control stream is plaintext XMPP with encrypted API bodies on the tested firmware; contact-to-cloud authentication uses verified STARTTLS.

### Taking over the device connection

Prefer a DNS override for the original Bosch hostname scoped to the thermostat or its IoT resolver. Keep `--domain` set to the original domain. If `both` uses that overridden resolver, set `--upstream` to a real Bosch IP:port or a resolver path unaffected by the override, otherwise it will loop back to itself. Global overrides can also affect the phone app. Device DNS caching, additional bootstrap names and any hardcoded IPs have not yet been tested: a gateway DNS capture is required before DNS takeover can be called reliable.

An optional device-scoped UDP/TCP DNS endpoint answers A/AAAA and XMPP SRV questions:

```sh
nefit serve --mode offline --device-ip 192.168.156.96 \
  --xmpp-listen 10.65.0.27:5222 \
  --dns-listen 10.65.0.27:53 --dns-address 10.65.0.27
```

Configure the device's resolver/DHCP DNS to use it. Unrelated names are refused unless `--dns-forward resolver:53` is set. Missing address-family records return no data, preventing an IPv6 record from bypassing a local IPv4 override. DNS proxy peers can be admitted explicitly through `server.DNSConfig.AllowedPeers` when embedding. The DNS endpoint does not advertise DHCP or take over an existing resolver automatically.

If DNS cannot be changed, redirect the thermostat's outbound TCP/5222 to the XMPP listener. On UniFi, use destination NAT on IoT ingress: source device IP, destination any, TCP port 5222, translated local server IP. Allow that device to reach the listener across VLANs. Changing the destination IP match alone does not redirect the cloud connection. A dedicated access point/router with its own DNS is another option when the main router cannot be configured. ARP spoofing and rogue DHCP are not supported installation methods.

A NixOS module is available as `nixosModules.default`:

```nix
services.nefit-go = {
  enable = true;
  package = nefit-go.packages.${pkgs.system}.default;
  environmentFile = "/run/secrets/nefit-env";
  mode = "both";
  deviceIP = "192.168.156.96";
  xmppAddress = "10.65.0.27:5222";
  updates = "block";
};
```

Configure network firewall access separately so only the thermostat reaches the XMPP listener.

### High-Level API

The library provides convenient methods for common operations:

```go
// Get system status
status, err := client.Status(ctx, includeOutdoorTemp)

// Get system pressure
pressure, err := client.Pressure(ctx)

// Set temperature
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

**Problem: HTTP 400 Bad Request on SetUserMode**

The API only accepts `"manual"` or `"clock"` as valid mode values. **`"off"` is NOT valid** and will cause a 400 error.

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
