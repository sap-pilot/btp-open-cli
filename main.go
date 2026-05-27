/*
Copyright © 2026 NAME HERE <EMAIL ADDRESS>

*/
package main

import (
	"btp-open-cli/cmd"
	_ "btp-open-cli/cmd/custom" // loads any custom commands registered via cmd.RegisterCommand
)

func main() {
	cmd.Execute()
}
