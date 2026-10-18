// Package main is a placeholder for uniadm, the UniSearch administrative
// tool (es-compat scan, benchmarks, backup/restore; design 00-overview §4).
// Subcommands arrive with later milestones.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "uniadm: not implemented yet; planned subcommands: es-compat scan, bench, backup, restore")
	os.Exit(1)
}
