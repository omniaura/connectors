package spec_test

import (
	"strings"
	"testing"

	"github.com/omniaura/connectors/spec"
)

func TestRejectsSwagger2AndNonOpenAPI(t *testing.T) {
	if _, err := spec.Parse([]byte(`{"swagger":"2.0","paths":{}}`)); err == nil || !strings.Contains(err.Error(), "swagger 2.0") {
		t.Fatalf("swagger 2.0: %v", err)
	}
	if _, err := spec.Parse([]byte(`{"hello":"world"}`)); err == nil {
		t.Fatal("accepted a non-OpenAPI document")
	}
}

func TestYAMLAndSelfReferenceTerminate(t *testing.T) {
	doc, err := spec.Parse([]byte(`
openapi: 3.0.3
servers: [{url: "https://api.example.com/v2"}]
paths:
  /loop:
    parameters:
      - {name: shared, in: query, schema: {type: string}}
    post:
      operationId: makeLoop
      parameters:
        - {name: shared, in: query, required: true, schema: {type: integer}}
      requestBody:
        content:
          application/vnd.api+json:
            schema: {$ref: "#/components/schemas/A"}
components:
  schemas:
    A: {type: object, properties: {b: {$ref: "#/components/schemas/B"}}}
    B: {type: object, properties: {a: {$ref: "#/components/schemas/A"}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Servers) != 1 || doc.Servers[0] != "https://api.example.com/v2" {
		t.Fatalf("servers = %v", doc.Servers)
	}
	if len(doc.Operations) != 1 {
		t.Fatalf("operations = %+v skipped = %+v", doc.Operations, doc.Skipped)
	}
	op := doc.Operations[0]
	if len(op.Parameters) != 1 || !op.Parameters[0].Required || op.Parameters[0].Schema["type"] != "integer" {
		t.Fatalf("operation parameter should override the path-level one: %+v", op.Parameters)
	}
	if op.Body == nil || op.Body.ContentType != "application/vnd.api+json" {
		t.Fatalf("+json media types are JSON: %+v", op.Body)
	}
	if op.ShortID() != "makeLoop" {
		t.Fatalf("non-FastAPI ids are kept: %s", op.ShortID())
	}
}

func TestMissingRefBecomesAnySchema(t *testing.T) {
	doc, err := spec.Parse([]byte(`{"openapi":"3.1.0","paths":{"/x":{"get":{"parameters":[{"name":"q","in":"query","schema":{"$ref":"#/components/schemas/Nope"}}]}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Operations[0].Parameters[0].Schema) != 0 {
		t.Fatalf("schema = %v", doc.Operations[0].Parameters[0].Schema)
	}
}
