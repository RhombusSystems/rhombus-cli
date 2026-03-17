package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <openapi.json> <output-dir>\n", os.Args[0])
		os.Exit(1)
	}

	specPath := os.Args[1]
	outDir := os.Args[2]

	fmt.Printf("Parsing OpenAPI spec: %s\n", specPath)
	services, err := ParseSpec(specPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing spec: %v\n", err)
		os.Exit(1)
	}

	totalOps := 0
	for _, svc := range services {
		totalOps += len(svc.Operations)
	}
	fmt.Printf("Found %d services with %d total operations\n", len(services), totalOps)

	fmt.Printf("Generating code to: %s\n", outDir)
	if err := WriteGeneratedFiles(services, outDir); err != nil {
		fmt.Fprintf(os.Stderr, "Error generating code: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Successfully generated %d service files + register.go\n", len(services))
}
