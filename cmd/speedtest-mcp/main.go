package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/mcpbridge"
)

var runBridge = mcpbridge.Run
var exitProcess = os.Exit

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitProcess(1)
	}
}

func run() error {
	return runWith(func() error { return godotenv.Load() })
}

func runWith(loadEnvironment func() error) error {
	if err := loadEnvironment(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("could not load Speedtest MCP environment")
	}
	return runBridge(context.Background(), os.Getenv("SPEEDTEST_MCP_ENDPOINT"), os.Getenv("SPEEDTEST_API_TOKEN"))
}
