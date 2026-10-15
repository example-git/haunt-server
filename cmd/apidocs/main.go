// Command apidocs exports generated client-development contracts without
// configuration, a database connection, network listeners or credentials.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"github.com/example-git/haunt-server/internal/httpapi"
	"os"
	"path/filepath"
)

func main() {
	out := flag.String("out", "docs", "output directory")
	clientOut := flag.String("client-out", "", "also export JSON contracts for standalone client builds")
	check := flag.Bool("check", false, "fail if checked-in documents differ")
	flag.Parse()
	docs, err := httpapi.GenerateDocuments()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !*check {
		if err := os.MkdirAll(*out, 0755); err != nil {
			panic(err)
		}
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{"openapi.json", docs.OpenAPI}, {"websocket.json", docs.WebSocket}, {"endpoints.md", docs.Endpoints}} {
		directories := []string{*out}
		if *clientOut != "" && filepath.Ext(file.name) == ".json" {
			directories = append(directories, *clientOut)
		}
		for _, directory := range directories {
			path := filepath.Join(directory, file.name)
			if *check {
				existing, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(existing, file.data) {
					fmt.Fprintln(os.Stderr, "Generated API documentation is stale:", path, "; run go generate ./internal/httpapi")
					os.Exit(1)
				}
			} else {
				if err := os.MkdirAll(directory, 0755); err != nil {
					panic(err)
				}
				if err := os.WriteFile(path, file.data, 0644); err != nil {
					panic(err)
				}
			}
		}
	}
}
