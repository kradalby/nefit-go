package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/kradalby/nefit-go/client"
	"github.com/peterbourgon/ff/v3/ffcli"
)

var (
	// Global flags
	rootFlagSet    = flag.NewFlagSet("nefit", flag.ExitOnError)
	serialNumber   = rootFlagSet.String("serial", os.Getenv("NEFIT_SERIAL_NUMBER"), "Serial number (or NEFIT_SERIAL_NUMBER env)")
	accessKey      = rootFlagSet.String("access-key", os.Getenv("NEFIT_ACCESS_KEY"), "Access key (or NEFIT_ACCESS_KEY env)")
	password       = rootFlagSet.String("password", os.Getenv("NEFIT_PASSWORD"), "Password (or NEFIT_PASSWORD env)")
	timeout        = rootFlagSet.Duration("timeout", 30*time.Second, "Request timeout")
	pretty         = rootFlagSet.Bool("pretty", false, "Pretty-print JSON output")
	verbose        = rootFlagSet.Bool("verbose", false, "Verbose output")
)

func main() {
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
  nefit status                      # Get system status
  nefit get /ecus/rrc/uiStatus     # Raw GET request
  nefit set temperature 21.5        # Set temperature to 21.5°C
  nefit pressure                    # Get system pressure`,
		FlagSet: rootFlagSet,
		Subcommands: []*ffcli.Command{
			statusCmd,
			pressureCmd,
			getCmd,
			putCmd,
			setCmd,
			hotWaterCmd,
			subscribeCmd,
			versionCmd,
		},
		Exec: func(ctx context.Context, args []string) error {
			return flag.ErrHelp
		},
	}

	if err := root.ParseAndRun(context.Background(), os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// Helper functions

func createClient() (*client.Client, error) {
	if *serialNumber == "" {
		return nil, fmt.Errorf("serial number required (--serial or NEFIT_SERIAL_NUMBER)")
	}
	if *accessKey == "" {
		return nil, fmt.Errorf("access key required (--access-key or NEFIT_ACCESS_KEY)")
	}
	if *password == "" {
		return nil, fmt.Errorf("password required (--password or NEFIT_PASSWORD)")
	}

	config := client.Config{
		SerialNumber: *serialNumber,
		AccessKey:    *accessKey,
		Password:     *password,
	}

	return client.NewClient(config)
}

func printJSON(v interface{}) error {
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
