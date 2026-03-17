package main

import "github.com/RhombusSystems/rhombus-cli/cmd"

var version = "dev"

func main() {
	cmd.SetVersion(version)
	cmd.Execute()
}
