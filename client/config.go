package client

import (
	"fmt"
	"time"
)

const (
	// DefaultHost is the Bosch XMPP server
	DefaultHost = "wa2-mz36-qrmzh6.bosch.de"

	// DefaultPort is the XMPP client-to-server port
	DefaultPort = 5222

	// AccessKeyPrefix is prepended to the access key for authentication
	AccessKeyPrefix = "Ct7ZR03b_"

	// RRCContactPrefix is used for the JID (from address)
	RRCContactPrefix = "rrccontact_"

	// RRCGatewayPrefix is used for the resource (to address)
	RRCGatewayPrefix = "rrcgateway_"

	// DefaultPingInterval is how often to send keepalive pings
	DefaultPingInterval = 30 * time.Second

	// DefaultMaxRetries is the maximum number of retry attempts
	DefaultMaxRetries = 15

	// DefaultRetryTimeout is the timeout for each request attempt
	DefaultRetryTimeout = 2 * time.Second
)

// Config holds the configuration for a Nefit Easy client
type Config struct {
	// Serial number of the device
	SerialNumber string

	// Access key (obtained from the device)
	AccessKey string

	// Password (user-set password)
	Password string

	// XMPP host (default: wa2-mz36-qrmzh6.bosch.de)
	Host string

	// XMPP port (default: 5222)
	Port int

	// PingInterval for keepalive (default: 30s)
	PingInterval time.Duration

	// MaxRetries for failed requests (default: 15)
	MaxRetries int

	// RetryTimeout per request (default: 2s)
	RetryTimeout time.Duration
}

// Validate checks if the config has required fields
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
	return nil
}

// WithDefaults returns a new config with default values filled in
func (c Config) WithDefaults() Config {
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
	return c
}

// JID returns the "from" JID (rrccontact_SERIAL@HOST)
func (c *Config) JID() string {
	return fmt.Sprintf("%s%s@%s", RRCContactPrefix, c.SerialNumber, c.Host)
}

// ResourceJID returns the "to" resource JID (rrcgateway_SERIAL@HOST)
func (c *Config) ResourceJID() string {
	return fmt.Sprintf("%s%s@%s", RRCGatewayPrefix, c.SerialNumber, c.Host)
}

// AuthPassword returns the password with the access key prefix
func (c *Config) AuthPassword() string {
	return AccessKeyPrefix + c.AccessKey
}
