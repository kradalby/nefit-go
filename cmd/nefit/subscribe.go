package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/peterbourgon/ff/v3/ffcli"
)

var subscribeCmd = &ffcli.Command{
	Name:       "subscribe",
	ShortUsage: "nefit subscribe",
	ShortHelp:  "Subscribe to all backend push notifications (debug)",
	LongHelp: `Subscribe to all backend push notifications and print them as they arrive.

This is a debug command that keeps the connection open and listens for
any updates from the Nefit Easy backend. This can include:
  - Temperature changes
  - Mode changes
  - Status updates
  - Any other state changes from the thermostat

The command will run until you press Ctrl+C.

Example:
  nefit subscribe
  nefit subscribe --pretty`,
	Exec: func(ctx context.Context, args []string) error {
		c, err := createClient()
		if err != nil {
			return err
		}
		defer c.Close() //nolint:errcheck

		if err := connectClient(c); err != nil {
			return err
		}

		fmt.Println("Connected and subscribed to push notifications.")
		fmt.Println("Listening for updates... (press Ctrl+C to exit)")
		fmt.Println()

		// Subscribe to all events
		c.Subscribe(func(uri string, data interface{}) {
			timestamp := time.Now().Format("15:04:05")

			if *pretty {
				// Pretty print JSON
				jsonData, err := json.MarshalIndent(map[string]interface{}{
					"timestamp": timestamp,
					"uri":       uri,
					"data":      data,
				}, "", "  ")
				if err != nil {
					fmt.Fprintf(os.Stderr, "[%s] ERROR: Failed to format data: %v\n", timestamp, err)
					return
				}
				fmt.Println(string(jsonData))
			} else {
				// Compact print
				jsonData, err := json.Marshal(data)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[%s] ERROR: Failed to format data: %v\n", timestamp, err)
					return
				}
				if uri != "" {
					fmt.Printf("[%s] %s: %s\n", timestamp, uri, string(jsonData))
				} else {
					fmt.Printf("[%s] %s\n", timestamp, string(jsonData))
				}
			}
		})

		// Wait for interrupt signal
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

		select {
		case <-sigChan:
			fmt.Println("\nReceived interrupt, shutting down...")
		case <-ctx.Done():
			fmt.Println("\nContext cancelled, shutting down...")
		}

		return nil
	},
}
