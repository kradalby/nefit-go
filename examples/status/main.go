package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/kradalby/nefit-go/client"
)

func main() {
	// Read credentials from environment variables
	serialNumber := os.Getenv("NEFIT_SERIAL_NUMBER")
	accessKey := os.Getenv("NEFIT_ACCESS_KEY")
	password := os.Getenv("NEFIT_PASSWORD")

	if serialNumber == "" || accessKey == "" || password == "" {
		log.Fatal("Please set NEFIT_SERIAL_NUMBER, NEFIT_ACCESS_KEY, and NEFIT_PASSWORD environment variables")
	}

	// Create client configuration
	config := client.Config{
		SerialNumber: serialNumber,
		AccessKey:    accessKey,
		Password:     password,
	}

	// Create client
	c, err := client.NewSimpleClient(config)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
	defer c.Close()

	// Connect to the Nefit Easy backend
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Println("Connecting to Nefit Easy...")
	if err := c.Connect(ctx); err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	fmt.Println("Connected successfully!")

	// Get system status
	fmt.Println("\nFetching system status...")
	status, err := c.Status(ctx, true) // Include outdoor temperature
	if err != nil {
		log.Fatalf("Failed to get status: %v", err)
	}

	// Print status as JSON
	statusJSON, _ := json.MarshalIndent(status, "", "  ")
	fmt.Println("\nSystem Status:")
	fmt.Println(string(statusJSON))

	// Get system pressure
	fmt.Println("\nFetching system pressure...")
	pressure, err := c.Pressure(ctx)
	if err != nil {
		log.Fatalf("Failed to get pressure: %v", err)
	}

	fmt.Printf("\nSystem Pressure: %.2f %s\n", pressure.Pressure, pressure.Unit)

	// Get hot water supply status
	fmt.Println("\nFetching hot water supply status...")
	hotWater, err := c.HotWaterSupply(ctx)
	if err != nil {
		log.Fatalf("Failed to get hot water supply: %v", err)
	}

	fmt.Printf("Hot Water Supply: %v\n", hotWater)

	fmt.Println("\nAll operations completed successfully!")
}
