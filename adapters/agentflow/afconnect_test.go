package afconnect_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/omniaura/agentflow/runtime/afagent"
	"github.com/omniaura/connectors"
	afconnect "github.com/omniaura/connectors/adapters/agentflow"
	"github.com/omniaura/connectors/spec"
)

func TestCatalogRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"id":"`+r.URL.Path+`"}`)
	}))
	defer srv.Close()
	doc, err := spec.Parse([]byte(`{"openapi":"3.1.0","paths":{"/items/{id}":{"get":{"operationId":"getItem","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}]}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	c, err := connectors.New(connectors.Manifest{Slug: "demo", Spec: "inline", BaseURL: "https://demo.example", Auth: connectors.Auth{Type: connectors.AuthBearer}}, doc, connectors.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	catalog, err := afagent.PrepareTools(ctx, nil, afconnect.Specs(c), nil)
	if err != nil {
		t.Fatalf("agentflow refused the generated schemas: %v", err)
	}
	if len(catalog.Tools) != 1 || catalog.Tools[0].Name != "DEMO_GET_ITEM" {
		t.Fatalf("tools = %+v", catalog.Tools)
	}
	dispatch := afconnect.Dispatcher(c, func(context.Context) (connectors.Credentials, error) {
		return connectors.Credentials{"api_key": "k"}, nil
	})
	out, err := catalog.Dispatch(ctx, afagent.ToolCall{ID: "1", Name: "DEMO_GET_ITEM", Arguments: `{"id":"42"}`}, nil, dispatch)
	if err != nil || out != `{"id":"/items/42"}` {
		t.Fatalf("out = %q err = %v", out, err)
	}
	// A wrong argument comes back as text the model can act on.
	out, err = catalog.Dispatch(ctx, afagent.ToolCall{ID: "2", Name: "DEMO_GET_ITEM", Arguments: `{"id":"1","colour":"red"}`}, nil, dispatch)
	if err != nil || !strings.Contains(out, "colour is not an argument of this tool") {
		t.Fatalf("out = %q err = %v", out, err)
	}
}
