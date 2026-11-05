// Command connectors lists, checks and calls connector tools.
//
//	connectors list
//	connectors tools <slug|manifest.yaml>
//	connectors check <slug|manifest.yaml>
//	connectors call  <slug|manifest.yaml> <TOOL_NAME> '<json arguments>'
//
// Credentials are read from CONNECTOR_<FIELD> environment variables
// (CONNECTOR_API_KEY for the default api_key field), never from arguments,
// so they stay out of shell history and process listings.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/omniaura/connectors"
	"github.com/omniaura/connectors/manifests"
	"github.com/omniaura/go-kit/errs"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "connectors:", describe(err))
		os.Exit(1)
	}
}

func describe(err error) string {
	var e *errs.Error
	if errors.As(err, &e) {
		out := e.Message()
		if code := e.Code(); code != "" {
			out += " (" + code + ")"
		}
		p := e.Public()
		for _, name := range slices.Sorted(maps.Keys(p.Fields)) {
			out += "; " + name + " " + p.Fields[name]
		}
		if d := e.Detail(); d != "" {
			out += ": " + d
		}
		return out
	}
	return err.Error()
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usage()
	}
	if args[0] == "list" {
		for _, s := range manifests.Slugs() {
			fmt.Println(s)
		}
		return nil
	}
	if len(args) < 2 {
		return usage()
	}
	c, err := load(ctx, args[1])
	if err != nil {
		return err
	}
	switch args[0] {
	case "tools":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(c.Tools()); err != nil {
			return err
		}
		for _, s := range c.Skipped() {
			fmt.Fprintf(os.Stderr, "skipped %s %s (%s): %s\n", s.Method, s.Path, s.OperationID, s.Reason)
		}
		return nil
	case "check":
		if err := c.Check(ctx, credentials(c.Manifest())); err != nil {
			return err
		}
		fmt.Println("credentials accepted")
		return nil
	case "call":
		if len(args) < 3 {
			return usage()
		}
		raw := "{}"
		if len(args) > 3 {
			raw = args[3]
		}
		r, err := c.Execute(ctx, credentials(c.Manifest()), args[2], json.RawMessage(raw))
		if err != nil {
			return err
		}
		fmt.Println(r.String())
		if !r.OK() {
			return fmt.Errorf("app answered %d", r.Status)
		}
		return nil
	}
	return usage()
}

func load(ctx context.Context, ref string) (*connectors.Connector, error) {
	if strings.HasSuffix(ref, ".yaml") || strings.HasSuffix(ref, ".yml") || strings.HasSuffix(ref, ".json") {
		data, err := os.ReadFile(ref)
		if err != nil {
			return nil, err
		}
		m, err := connectors.ParseManifest(data)
		if err != nil {
			return nil, err
		}
		return connectors.Load(ctx, m, connectors.WithSpecDir(filepath.Dir(ref)))
	}
	m, err := manifests.Get(ref)
	if err != nil {
		return nil, err
	}
	return connectors.Load(ctx, m)
}

func credentials(m connectors.Manifest) connectors.Credentials {
	out := connectors.Credentials{}
	for _, f := range m.Auth.Fields {
		if v := os.Getenv("CONNECTOR_" + strings.ToUpper(f.Name)); v != "" {
			out[f.Name] = v
		}
	}
	return out
}

func usage() error {
	return errors.New(`usage:
  connectors list
  connectors tools <slug|manifest.yaml>
  connectors check <slug|manifest.yaml>          (credentials from CONNECTOR_<FIELD>)
  connectors call  <slug|manifest.yaml> <TOOL> '<json>'`)
}
