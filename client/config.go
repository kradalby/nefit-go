package client

import (
	"fmt"
	"strings"
	"time"
)

const (
	DefaultHost = "wa2-mz36-qrmzh6.bosch.de"
	DefaultPort = 5222

	// AccessKeyPrefix is prepended to the access key for authentication
	AccessKeyPrefix = "Ct7ZR03b_"

	RRCContactPrefix = "rrccontact_"
	RRCGatewayPrefix = "rrcgateway_"

	DefaultPingInterval = 30 * time.Second
	DefaultMaxRetries   = 3 // PUT retries back off exponentially
	DefaultRetryTimeout = 2 * time.Second

	DefaultConnectTimeout = 30 * time.Second
)

// Config holds the configuration for a Nefit Easy client.
type Config struct {
	SerialNumber string
	AccessKey    string
	Password     string

	Host         string
	Port         int
	PingInterval time.Duration
	MaxRetries   int
	RetryTimeout time.Duration

	// ConnectTimeout bounds a login. It runs apart from the request that
	// triggered it, which may stop waiting sooner, so it needs its own bound.
	ConnectTimeout time.Duration
}

// Validate requires credentials and rejects negative timeouts, intervals and
// retries.
func (c *Config) Validate() error {
	if c.SerialNumber == "" {
		return fmt.Errorf("serial number is required")
	}
	if c.AccessKey == "" {
		return fmt.Errorf("access key is required")
	}
	if c.Password == "" {
		return fmt.Errorf("password is required")
	}
	if c.ConnectTimeout < 0 || c.RetryTimeout < 0 || c.PingInterval < 0 || c.MaxRetries < 0 {
		// Zero means the default; a negative one would expire every login
		// at once or panic the ping ticker.
		return fmt.Errorf("timeouts, intervals and retries must not be negative")
	}
	return nil
}

// WithDefaults returns a copy of the config with unset fields populated from defaults.
func (c Config) WithDefaults() Config {
	// The device names the domain in lower case without the root dot, and
	// its login must match exactly.
	c.Host = strings.TrimSuffix(strings.ToLower(c.Host), ".")
	if c.Host == "" {
		c.Host = DefaultHost
	}
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.PingInterval == 0 {
		c.PingInterval = DefaultPingInterval
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = DefaultMaxRetries
	}
	if c.RetryTimeout == 0 {
		c.RetryTimeout = DefaultRetryTimeout
	}
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = DefaultConnectTimeout
	}
	return c
}

// JID returns the client JID used as the "from" address in XMPP messages.
// Format: rrccontact_SERIAL@HOST
func (c *Config) JID() string {
	return fmt.Sprintf("%s%s@%s", RRCContactPrefix, c.SerialNumber, c.Host)
}

// ResourceJID returns the backend JID used as the "to" address in XMPP messages.
// Format: rrcgateway_SERIAL@HOST
func (c *Config) ResourceJID() string {
	return fmt.Sprintf("%s%s@%s", RRCGatewayPrefix, c.SerialNumber, c.Host)
}

// AuthPassword returns the authentication password by prepending the required prefix to the access key.
func (c *Config) AuthPassword() string {
	return AccessKeyPrefix + c.AccessKey
}
