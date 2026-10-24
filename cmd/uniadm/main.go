// uniadm is the UniSearch administrative tool (design 00-overview §4).
// Implemented subcommands:
//
//	es-compat scan — assess an ES mapping (and optional query log)
//	                 for UniSearch compatibility.
//
// Planned (later milestones): bench, backup, restore.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/unisearch-db/unisearch/cmd/uniadm/escompat"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "es-compat":
		if len(os.Args) < 3 || os.Args[2] != "scan" {
			fmt.Fprintln(os.Stderr, "usage: uniadm es-compat scan --mapping <file> | --url <base> --index <name> [--queries <file>]")
			os.Exit(1)
		}
		if err := runEsCompatScan(os.Args[3:]); err != nil {
			fmt.Fprintln(os.Stderr, "uniadm:", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "uniadm: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `uniadm — UniSearch administrative tool

Usage:
  uniadm es-compat scan --mapping <file> | --url <base-url> --index <name>
                        [--queries <file>] [--out <file>]

    Assess an Elasticsearch mapping (and an optional query log, one
    JSON search request per line) for UniSearch compatibility and
    print a JSON report with issues, scores, and suggestions.

    --mapping <file>   ES mapping JSON file (alternative to --url)
    --url <base-url>   Cluster base URL, e.g. http://localhost:9200
                       (fetches GET {url}/{index}/_mapping)
    --index <name>     Index name (required with --url)
    --queries <file>   Optional query log, one search body per line
    --out <file>       Write the report to a file instead of stdout

Planned subcommands: bench, backup, restore
`)
}

func runEsCompatScan(args []string) error {
	fs := flag.NewFlagSet("es-compat scan", flag.ContinueOnError)
	mappingFile := fs.String("mapping", "", "path to an ES mapping JSON file")
	baseURL := fs.String("url", "", "cluster base URL, e.g. http://localhost:9200")
	indexName := fs.String("index", "", "index name (required with --url)")
	queriesFile := fs.String("queries", "", "path to a query log file (one search body per line)")
	outFile := fs.String("out", "", "write the report to a file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *mappingFile == "" && (*baseURL == "" || *indexName == "") {
		return fmt.Errorf("either --mapping <file> or --url <base> --index <name> is required")
	}

	// Load the mapping (file or cluster).
	var mappingJSON []byte
	var err error
	switch {
	case *mappingFile != "":
		mappingJSON, err = os.ReadFile(*mappingFile)
		if err != nil {
			return fmt.Errorf("read mapping: %w", err)
		}
	default:
		mappingJSON, err = fetchMapping(*baseURL, *indexName)
		if err != nil {
			return fmt.Errorf("fetch mapping: %w", err)
		}
	}

	name := *indexName
	if name == "" {
		name = strings.TrimSuffix(*mappingFile, ".json")
	}

	// Load the optional query log.
	var queryLogs []string
	if *queriesFile != "" {
		data, err := os.ReadFile(*queriesFile)
		if err != nil {
			return fmt.Errorf("read queries: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				queryLogs = append(queryLogs, line)
			}
		}
	}

	report, err := escompat.Run(name, mappingJSON, queryLogs)
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if *outFile != "" {
		f, err := os.Create(*outFile)
		if err != nil {
			return fmt.Errorf("create out: %w", err)
		}
		defer f.Close()
		enc = json.NewEncoder(f)
	}
	return enc.Encode(report)
}

// fetchMapping retrieves GET {base}/{index}/_mapping from a cluster.
func fetchMapping(baseURL, index string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	url := strings.TrimRight(baseURL, "/") + "/" + index + "/_mapping"
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	return io.ReadAll(resp.Body)
}
