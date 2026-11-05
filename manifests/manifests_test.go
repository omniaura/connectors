package manifests_test

import (
	"context"
	"os"
	"testing"

	"github.com/omniaura/connectors"
	"github.com/omniaura/connectors/manifests"
)

func TestEmbeddedManifestsValidate(t *testing.T) {
	slugs := manifests.Slugs()
	if len(slugs) == 0 {
		t.Fatal("no embedded manifests")
	}
	for _, slug := range slugs {
		m, err := manifests.Get(slug)
		if err != nil {
			t.Fatalf("%s: %v", slug, err)
		}
		if m.Slug != slug {
			t.Fatalf("%s.yaml declares slug %q", slug, m.Slug)
		}
	}
	t.Logf("validated %d manifests: %v", len(slugs), slugs)
}

// TestLiveBluePillow talks to the real API. It runs only when
// CONNECTORS_LIVE=1 and CONNECTOR_API_KEY holds an anonymous B2A key.
func TestLiveBluePillow(t *testing.T) {
	key := os.Getenv("CONNECTOR_API_KEY")
	if os.Getenv("CONNECTORS_LIVE") != "1" || key == "" {
		t.Skip("set CONNECTORS_LIVE=1 and CONNECTOR_API_KEY to run")
	}
	ctx := context.Background()
	m, err := manifests.Get("bluepillow")
	if err != nil {
		t.Fatal(err)
	}
	c, err := connectors.Load(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range c.Tools() {
		if tool.Name == "BLUEPILLOW_CREATE_KEY" || tool.Name == "BLUEPILLOW_HEALTH" {
			t.Fatalf("%s must be excluded", tool.Name)
		}
	}
	t.Logf("%d tools, %d skipped operations", len(c.Tools()), len(c.Skipped()))
	creds := connectors.Credentials{"api_key": key}
	if err := c.Check(ctx, creds); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	if err := c.Check(ctx, connectors.Credentials{"api_key": "pk_anon_not_a_real_key"}); !connectors.ErrCredentialsRejected.Is(err) {
		t.Fatalf("bogus key: %v", err)
	}
	r, err := c.Execute(ctx, creds, "BLUEPILLOW_RESOLVE_DESTINATION", []byte(`{"name":"Paris"}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("resolve destination: status %d, %d bytes", r.Status, len(r.Body))
	if !r.OK() {
		t.Fatalf("resolve destination: %s", r.String())
	}
}
