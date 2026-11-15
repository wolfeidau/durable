package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/config"
)

type configCmd struct {
	Check  configCheckCmd  `cmd:"" help:"Validate ~/.durable/config, mcp and hooks (.json or .yaml) and print the effective settings with their sources. Never resolves a secret or contacts a host."`
	Schema configSchemaCmd `cmd:"" help:"Print the JSON Schema of ~/.durable/config.json and config.yaml."`
}

type configCheckCmd struct{}

type configSchemaCmd struct{}

func (*configCheckCmd) Run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to find home directory: %w", err)
	}
	settings, err := config.Load(home)
	if err != nil {
		return err
	}
	err = settings.Validate()
	for _, w := range settings.Warnings {
		fmt.Printf("warning: %s\n", w.Message)
	}
	settings.Describe(os.Stdout)
	userWarnings, userErr := commands.CheckUserConfig(home)
	for _, w := range userWarnings {
		fmt.Printf("warning: %s\n", w)
	}
	return errors.Join(err, userErr)
}

func (*configSchemaCmd) Run() error {
	b, err := config.SchemaJSON()
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}
