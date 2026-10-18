// Package main implements the unisearch server binary (design 00-overview
// §4/§5): embedded single-node mode and, from M2 onward, cloud roles.
package main

import "os"

func main() {
	if err := newRootCommand().Execute(); err != nil {
		// cobra has already printed the error to stderr.
		os.Exit(1)
	}
}
