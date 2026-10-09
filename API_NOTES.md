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

`SetUserMode` rejects any other value before sending. A raw PUT of `"off"` reaches the device and gets:
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

PUT retries use exponential backoff. GET retries start immediately, and so does a PUT whose session ended before it was written: waiting for the next login already paces it. Both retry only failures known to occur before sending:

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

Retries occur when an attempt times out (`context.DeadlineExceeded`) before reaching the backend, or loses its session while still unsent. Examples include waiting in the queue, waiting for login, and losing a connection before obtaining its writer. Sent requests are not replayed.

Retries do NOT occur for:
- A request that went out and got no reply in time. Replies carry no request id, so its late reply could answer a retry, and a write must not be replayed. On the cloud transport the session is retired at once; the device server retires it only if the reply never arrives within `RequestTimeout`. The error still matches `context.DeadlineExceeded`.
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
DBG PUT request encrypted uri=/heatingCircuits/hc1/usermode encrypted_length=44
DBG sending PUT request uri=/heatingCircuits/hc1/usermode from=rrccontact_SERIAL@wa2-mz36-qrmzh6.bosch.de to=rrcgateway_SERIAL@wa2-mz36-qrmzh6.bosch.de encrypted_payload_length=44 decrypted_json={"value":"manual"}
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

**Valid range:** `SetTemperature` accepts `client.MinTemperature` (5.0 °C) to `client.MaxTemperature` (30.0 °C) and returns `client.ErrInvalidValue` for anything else, without sending. Your boiler configuration may narrow the range further.

## API Rate Limiting

The Nefit Easy backend only allows **one concurrent request at a time**. The library handles this automatically using a request queue.

**Important:** Do not create multiple client instances for the same boiler - they will interfere with each other.

## Error Handling Best Practices

1. **Check for specific errors:**
   ```go
   var httpErr *client.HTTPError
   switch {
   case errors.Is(err, protocol.ErrInvalidURI):
       // Not origin-form: no leading '/', or whitespace, control bytes, non-ASCII or '#'; nothing was sent
   case errors.Is(err, client.ErrInvalidValue):
       // Temperature or user mode out of range; nothing was sent
   case errors.Is(err, client.ErrUpdateBlocked):
       // Refused by UpdatesBlock before sending
   case errors.Is(err, context.DeadlineExceeded):
       // Timed out; the library already retried what never reached the device
   case errors.Is(err, client.ErrClosed), errors.Is(err, client.ErrListenerFailed):
       // The client (or server.Server) is done; create a new one
   case errors.As(err, &httpErr) && httpErr.StatusCode == 400:
       // The device rejected the request - fix the data
   }
   ```

2. **Enable debug logging during development** to see exactly what's being sent

3. **Use appropriate timeouts** - the default 2 second `RetryTimeout` works for most requests

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
- Increase `RetryTimeout` in config
- Verify network connectivity to `wa2-mz36-qrmzh6.bosch.de:5222`

## Connection Lifecycle

- The client owns connecting. A request with no live session logs in on its own; `Connect()` does the same ahead of time, which is what lets push notifications arrive before the first request.
- One login runs at a time. Requests and `Connect()` calls that need a session meanwhile wait for it rather than start another. A caller that gives up does not abort it; `ConnectTimeout` (default 30s) and `Close()` do.
- `Done()` is closed when the session ends: the stream fails, `Close()` is called, or, on the cloud transport, a request is abandoned after it may have gone out. To keep cloud pushes flowing, wait on `Done()` and call `Connect()` again, with backoff; the device server admits new logins by itself.
- At most 64 push handlers run at once. While all are busy, further pushes are dropped with a warning, so a slow handler cannot stall replies.
- Requests that expire before writing, including while waiting behind cloud requests in `both`, preserve the session and are retried while time remains. Update-policy rejections also preserve it. Writes that may have reached the backend are never replayed.
- Replies carry no request id. On the cloud transport, a request abandoned after sending retires the session, since its late reply could pass for the next one's. On the local device server the bridge tracks the outstanding request itself: an abandoned request keeps the device until its reply arrives (and is discarded) or `RequestTimeout` passes, and only an unanswered request retires the session.
- `Connect()` returning nil means a session was established; it may already have ended, so watch `Done()` rather than assume it is up.
- A half-open connection (the peer vanished without closing) is noticed only when a request goes unanswered, or when the kernel gives up retransmitting a keepalive presence, which takes minutes. There is no read deadline; pushes stop silently until then. Poll with a request to notice sooner. The local device server also replaces such a session as soon as the device completes a new login.
- The device server (`server.Server`, `nefit serve`) queues, relays and recovers as described under "Device server modes" in [README.md](README.md), which also lists its known limits.

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
- Review `types/types.go` for all available status fields
