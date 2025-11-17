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

