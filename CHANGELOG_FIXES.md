# Changelog - Production Fixes

## Changes Made - 2024

### 1. Enhanced Debug Logging for PUT Requests

**Problem:** When PUT requests failed in production, there was insufficient visibility into what was actually being sent to the API.

**Solution:** Added comprehensive debug logging at multiple stages:

- **Request Preparation:**
  - Log the URI being called
  - Log the decrypted JSON payload
  - Log the JSON payload length

- **Encryption Stage:**
  - Log the encrypted payload length

- **Transmission:**
  - Log full JID addresses (from/to)
  - Log the decrypted JSON for debugging
  - Log encrypted payload metadata

- **Response Handling:**
  - Log HTTP status codes
  - Log detailed error information including the JSON that was sent
  - Log successful request completion

- **Retry Logic:**
  - Log retry attempt number
  - Log backoff duration
  - Log the last error that triggered the retry

**Usage:**
```go
logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
}))
client.SetLogger(logger)
```

**Files Changed:**
- `client/client.go` - Enhanced `Put()` and `executePut()` with detailed logging
- `examples/debug-logging/main.go` - New example demonstrating debug logging

---

### 2. Fixed Invalid "off" Mode Issue

**Problem:** The API was receiving `"off"` as a user mode value, which is invalid and results in HTTP 400 Bad Request errors.

**Solution:** 

- Added explicit validation in `SetUserMode()` to reject invalid modes before sending to API
- Improved error messages to clearly state valid values: `"manual"` and `"clock"`
- Added specific note that `"off"` is NOT a valid mode
- Documented alternatives for turning off heating

**Valid Modes:**
- `"manual"` - User controls temperature directly
- `"clock"` - Follows programmed heating schedule

**Invalid:**
- `"off"` - Will cause HTTP 400 Bad Request

**Alternatives to turn off heating:**
```go
// Set manual mode with minimum temperature
client.SetUserMode(ctx, "manual")
client.SetTemperature(ctx, 5.0)

// Or disable hot water supply
client.SetHotWaterSupply(ctx, false)
```

**Files Changed:**
- `client/commands.go` - Enhanced `SetUserMode()` with validation and logging
- `types/uris.go` - Added documentation for `URIUserMode`

---

### 3. Implemented Exponential Backoff for Retries

**Problem:** The library was configured with 15 immediate retries. This was problematic because:
- HTTP 400 errors (invalid data) are permanent, not transient - retrying won't help
- Immediate retries waste resources and don't allow transient issues to resolve
- Hammering the API with rapid retries could cause rate limiting

**Solution:** Implemented exponential backoff with intelligent retry logic:

**Retry Strategy:**
- Initial backoff: 2 seconds (configurable via `RetryTimeout`)
- Backoff multiplier: 2x per attempt
- Maximum backoff: 30 seconds
- Default max retries: Reduced from 15 to 3

**Retry Timeline:**
- Attempt 1: Immediate
- Attempt 2: After 2 seconds
- Attempt 3: After 4 seconds  
- Attempt 4: After 8 seconds

**Smart Retry Logic:**
- **WILL retry:** Timeout errors and transient network failures
- **WON'T retry:** HTTP 400 (invalid data), 404 (not found), 500+ (server errors)

**Rationale:** 
- 400 errors mean the request format/values are wrong - retrying the same invalid request will never succeed
- Exponential backoff gives transient issues time to resolve
- Fewer retries with backoff is more efficient than many immediate retries

**Files Changed:**
- `client/client.go` - Implemented exponential backoff in `Put()` method
- `client/config.go` - Reduced `DefaultMaxRetries` from 15 to 3

---

### 4. Comprehensive API Documentation

**Problem:** Limited documentation on valid values, error handling, and troubleshooting.

**Solution:** Created comprehensive documentation covering:

- Valid values for all endpoints
- Common mistakes and how to avoid them
- Retry behavior and when retries happen
- Debug logging setup and interpretation
- HTTP status codes and their meanings
- Troubleshooting guide for common issues
- Production recommendations
- API limitations and best practices

**Files Created:**
- `API_NOTES.md` - Detailed API documentation with examples
- `examples/debug-logging/main.go` - Working example of debug logging

**Files Updated:**
- `README.md` - Added debugging section and link to API notes

---

## Testing

All changes have been tested:
```bash
go test ./...    # All tests pass
go build ./...   # Compiles successfully
```

---

## Migration Guide

### If you're using custom retry logic:

Before:
```go
config := client.Config{
    MaxRetries: 15,  // Old default
}
```

After (with exponential backoff):
```go
config := client.Config{
    MaxRetries: 3,   // New default - fewer retries but with backoff
}
```

### If you were trying to set mode to "off":

Before (BROKEN):
```go
client.SetUserMode(ctx, "off")  // Returns HTTP 400 error
```

After (FIXED):
```go
// Use manual mode with low temperature instead
client.SetUserMode(ctx, "manual")
client.SetTemperature(ctx, 5.0)
```

### To enable debug logging:

```go
import "log/slog"

logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
}))
client.SetLogger(logger)
```

---

## Summary

These changes improve production reliability by:

1. **Better visibility** - Debug logging shows exactly what's being sent
2. **Preventing invalid requests** - Validation catches errors before API calls
3. **Smarter retries** - Exponential backoff reduces unnecessary API calls
4. **Better documentation** - Clear guidance on valid values and troubleshooting

The library now fails fast on invalid data (400 errors) while gracefully handling transient failures with exponential backoff.