# Nefit Easy API Notes

This document contains important information about the Nefit Easy API behavior, valid values, and common pitfalls discovered during production use.

## User Mode Endpoint

**Endpoint:** `/heatingCircuits/hc1/usermode`

### Valid Values

The API only accepts the following values for the user mode:

- `"manual"` - Manual heating mode where the user controls temperature directly
- `"clock"` - Clock/scheduled mode that follows the programmed heating schedule

### Common Mistakes

**❌ INVALID:** `"off"` is NOT a valid mode value

Attempting to set the mode to `"off"` will result in:
```
HTTP 400 Bad Request
```

This is a permanent error (not transient), so retrying will not help.

### How to Turn Off Heating

Since `"off"` is not a valid mode, use one of these approaches:

1. **Set manual mode with low temperature:**
   ```go
   client.SetUserMode(ctx, "manual")
   client.SetTemperature(ctx, 5.0) // Minimum temperature
   ```

2. **Disable hot water supply:**
   ```go
   client.SetHotWaterSupply(ctx, false)
   ```

3. **Use fireplace mode** (if available on your system)

## Retry Behavior

### Exponential Backoff

The library now uses exponential backoff for retries instead of immediate retries:

- Initial retry timeout: 2 seconds (configurable via `RetryTimeout`)
- Backoff multiplier: 2x
- Maximum backoff: 30 seconds
- Default max retries: 3 (configurable via `MaxRetries`)

Example retry timeline:
- Attempt 1: Immediate
- Attempt 2: After 2 seconds
- Attempt 3: After 4 seconds
- Attempt 4: After 8 seconds

### When Retries Happen

Retries only occur for:
- Timeout errors (`context.DeadlineExceeded`)
- Network-related transient failures

Retries do NOT occur for:
- HTTP 400 Bad Request (indicates invalid data)
- HTTP 404 Not Found (indicates invalid endpoint)
- HTTP 500+ Server Errors (typically indicates API or boiler issues)

**Rationale:** If the API returns 400, it means the request format or values are wrong. Retrying the same invalid request will not succeed.

## Debug Logging

### Enabling Debug Logs

Set a custom logger with debug level enabled:

```go
import "log/slog"

logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
}))

client.SetLogger(logger)
```

### What Gets Logged

For PUT requests, the following information is logged at DEBUG level:

- **Request preparation:**
  - URI
  - Decrypted JSON data
  - JSON payload length

- **Encryption:**
  - Encrypted payload length

- **Sending:**
  - From/To JID addresses
  - Full decrypted JSON for debugging
  - Encrypted payload length

- **Response:**
  - HTTP status code
  - Status message

- **Retries:**
  - Retry attempt number
  - Backoff duration
  - Last error message

For failed requests, ERROR level logs include:
- The exact JSON data that was sent
- HTTP status code and message
- Full error context

### Example Debug Output

```
DBG PUT request data prepared uri=/heatingCircuits/hc1/usermode json_data={"value":"manual"} json_length=18
DBG PUT request encrypted uri=/heatingCircuits/hc1/usermode encrypted_length=24
DBG sending PUT request uri=/heatingCircuits/hc1/usermode from=rrccontact_SERIAL@wa2-mz36-qrmzh6.bosch.de to=rrcgateway_SERIAL@wa2-mz36-qrmzh6.bosch.de encrypted_payload_length=24 decrypted_json={"value":"manual"}
DBG PUT request successful uri=/heatingCircuits/hc1/usermode status_code=204
```

## Common HTTP Status Codes

| Code | Meaning | Action |
|------|---------|--------|
| 200 | Success with response body | Parse the response data |
| 204 | Success (No Content) | Request succeeded, no response data |
| 400 | Bad Request | Check the request format and values - **do not retry** |
| 404 | Not Found | Endpoint doesn't exist or is disabled on your system |
| 500 | Internal Server Error | API or boiler issue - may be transient |
| 503 | Service Unavailable | Backend temporarily unavailable - may be transient |

## Hot Water Supply

**Endpoints:**
- Manual mode: `/dhwCircuits/dhwA/dhwOperationManualMode`
- Clock mode: `/dhwCircuits/dhwA/dhwOperationClockMode`

**Valid values:**
- `"on"` - Hot water supply enabled
- `"off"` - Hot water supply disabled

**Important:** The endpoint used depends on the current user mode. The library automatically selects the correct endpoint.

## Temperature Control

### Manual Temperature Setpoint

Setting temperature requires THREE API calls:

1. Set manual setpoint temperature
2. Enable manual override status
3. Set override temperature

The `SetTemperature()` method handles all three calls automatically.

**Valid range:** Typically 5.0°C to 30.0°C (depends on your boiler configuration)

## API Rate Limiting

The Nefit Easy backend only allows **one concurrent request at a time**. The library handles this automatically using a request queue.

**Important:** Do not create multiple client instances for the same boiler - they will interfere with each other.

## Error Handling Best Practices

1. **Always check for specific error types:**
   ```go
   if err != nil {
       if strings.Contains(err.Error(), "HTTP error 400") {
           // Invalid request - fix the data
       } else if strings.Contains(err.Error(), "context deadline exceeded") {
           // Timeout - maybe retry manually
       }
   }
   ```

2. **Enable debug logging during development** to see exactly what's being sent

3. **Use appropriate timeouts** - the default 2 seconds works for most requests

4. **Don't retry 400 errors** - they indicate invalid input

## Troubleshooting

### Problem: Constant HTTP 400 errors

**Check:**
- Are you using valid mode values? (`"manual"` or `"clock"`, not `"off"`)
- Is the temperature in valid range?
- Is the endpoint correct for your boiler model?

**Enable debug logging** to see the exact JSON being sent.

### Problem: Timeout errors

**Possible causes:**
- Network connectivity issues
- Boiler is offline or unreachable
- XMPP connection dropped

**Solutions:**
- Check `IsConnected()` before making requests
- Increase `RetryTimeout` in config
- Verify network connectivity to `wa2-mz36-qrmzh6.bosch.de:5222`

### Problem: "Not connected" errors

**Solution:**
- Call `Connect()` before making requests
- Implement reconnection logic in your application
- Check for connection errors in the receive worker logs

## Production Recommendations

1. **Use structured logging (slog)** with appropriate levels
2. **Monitor error rates** - sudden increases may indicate API changes
3. **Implement graceful degradation** - don't crash if one request fails
4. **Keep request frequency reasonable** - avoid hammering the API
5. **Handle push notifications** - the boiler sends unsolicited updates
6. **Implement proper shutdown** - call `Close()` to clean up resources

## API Limitations

- One concurrent request per connection
- No bulk operations - each setting requires separate requests
- Some endpoints may not be available on all boiler models
- Push notification format may vary by firmware version
- No official API documentation from Bosch

## Further Reading

- See `examples/` directory for working code examples
- Check test files for additional API endpoint usage
- Review `types/status.go` for all available status fields