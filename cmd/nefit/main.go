package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/peterbourgon/ff/v3/ffcli"

	"github.com/kradalby/nefit-go/client"
)

var (
	// Global flags
	rootFlagSet  = flag.NewFlagSet("nefit", flag.ExitOnError)
	serialNumber = envStringFlag(rootFlagSet, "serial", "NEFIT_SERIAL_NUMBER", "Serial number (or NEFIT_SERIAL_NUMBER env)")
	accessKey    = envStringFlag(rootFlagSet, "access-key", "NEFIT_ACCESS_KEY", "Access key (or NEFIT_ACCESS_KEY env)")
	password     = envStringFlag(rootFlagSet, "password", "NEFIT_PASSWORD", "Password (or NEFIT_PASSWORD env)")
	timeout      = rootFlagSet.Duration("timeout", 30*time.Second, "Request timeout (serve: also the device answer window and HTTP drain)")
	pretty       = rootFlagSet.Bool("pretty", false, "Pretty-print JSON output")
	verbose      = rootFlagSet.Bool("verbose", false, "Verbose output")
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		var usage usageError
		var config configError
		switch {
		case errors.As(err, &config):
			os.Exit(exitConfig)
		case errors.As(err, &usage):
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// exitConfig is sysexits' EX_CONFIG. Not 2: the Go runtime exits 2 on a
// crash, which a supervisor should restart.
const exitConfig = 78

// configError exits exitConfig without usage, so a supervisor can stop
// restarting a service whose configuration cannot work
// (RestartPreventExitStatus).
type configError struct{ error }

// startupError keeps failures a restart can fix, such as a listen address
// not assigned yet or a port still held, out of configError. A port the
// user may not bind needs a configuration change.
func startupError(err error) error {
	var op *net.OpError
	var addr *net.AddrError
	if errors.As(err, &op) && !errors.As(err, &addr) && !errors.Is(err, syscall.EACCES) {
		return err
	}
	return configError{err}
}

func (e configError) Unwrap() error { return e.error }

// usageError makes ffcli print usage (it matches flag.ErrHelp) and main exit 2.
type usageError string

func (e usageError) Error() string      { return string(e) }
func (usageError) Is(target error) bool { return target == flag.ErrHelp }

func requireSubcommand(_ context.Context, args []string) error {
	if len(args) == 0 {
		return usageError("missing command")
	}
	return usageError(fmt.Sprintf("unknown command %q", args[0]))
}

func run(ctx context.Context, args []string) error {
	// Create root command
	root := &ffcli.Command{
		Name:       "nefit",
		ShortUsage: "nefit [flags] <subcommand>",
		ShortHelp:  "Nefit Easy CLI - Control your Nefit/Bosch thermostat",
		LongHelp: `A command-line interface for Nefit Easy thermostats.

Environment variables:
  NEFIT_SERIAL_NUMBER  Serial number of your device
  NEFIT_ACCESS_KEY     Access key from the mobile app
  NEFIT_PASSWORD       Your password

Examples:
  nefit status                  # Get system status
  nefit get /ecus/rrc/uiStatus  # Raw GET request
  nefit set temperature 21.5    # Set temperature to 21.5°C
  nefit pressure                # Get system pressure
  nefit serve --device-ip 192.0.2.10 --xmpp-listen 192.0.2.1:5222
                                # Run device server and HTTP API`,
		FlagSet: rootFlagSet,
		Subcommands: []*ffcli.Command{
			serveCmd,
			statusCmd,
			pressureCmd,
			getCmd,
			putCmd,
			setCmd,
			hotWaterCmd,
			subscribeCmd,
			versionCmd,
		},
		Exec: requireSubcommand,
	}

	if err := root.Parse(args); err != nil {
		return err
	}
	if *timeout <= 0 {
		return configError{errors.New("--timeout must be positive")}
	}
	return root.Run(ctx)
}

// Helper functions

func envStringFlag(fs *flag.FlagSet, name, env, usage string) *string {
	value := fs.String(name, "", usage)
	// Keep printable defaults empty without changing flag precedence.
	*value = os.Getenv(env)
	return value
}

func requireCredentials() error {
	switch {
	case *serialNumber == "":
		return fmt.Errorf("serial number required (--serial or NEFIT_SERIAL_NUMBER)")
	case *accessKey == "":
		return fmt.Errorf("access key required (--access-key or NEFIT_ACCESS_KEY)")
	case *password == "":
		return fmt.Errorf("password required (--password or NEFIT_PASSWORD)")
	}
	return nil
}

func createClient() (*client.Client, error) {
	if err := requireCredentials(); err != nil {
		return nil, err
	}

	config := client.Config{
		SerialNumber: *serialNumber,
		AccessKey:    *accessKey,
		Password:     *password,
	}

	return client.NewClient(config)
}

func printJSON(v any) error {
	var data []byte
	var err error

	if *pretty {
		data, err = json.MarshalIndent(v, "", "  ")
	} else {
		data, err = json.Marshal(v)
	}

	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	fmt.Println(string(data))
	return nil
}

func connectClient(c *client.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if *verbose {
		fmt.Fprintln(os.Stderr, "Connecting to Nefit Easy...")
	}

	if err := c.Connect(ctx); err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}

	if *verbose {
		fmt.Fprintln(os.Stderr, "Connected successfully")
	}

	return nil
}

// Version command
var versionCmd = &ffcli.Command{
	Name:       "version",
	ShortUsage: "nefit version",
	ShortHelp:  "Print version information",
	Exec: func(ctx context.Context, args []string) error {
		fmt.Println("nefit version 0.1.0 (validated)")
		fmt.Println("Go implementation of Nefit Easy protocol")
		return nil
	},
}
