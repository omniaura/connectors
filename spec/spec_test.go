package spec_test

import (
	"fmt"
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

// Each nesting level used to count toward the ref limit, so a FastAPI body a
// few models deep came out with "type": {} — an invalid schema.
func TestDeepModelsKeepTheirTypes(t *testing.T) {
	schemas := `"L6":{"type":"object","properties":{"leaf":{"type":"string","enum":["a","b"]}},"required":["leaf"]}`
	for i := 5; i >= 0; i-- {
		schemas += fmt.Sprintf(`,"L%d":{"type":"object","properties":{"next":{"anyOf":[{"$ref":"#/components/schemas/L%d"},{"type":"null"}]}}}`, i, i+1)
	}
	doc, err := spec.Parse([]byte(`{"openapi":"3.1.0","paths":{"/deep":{"post":{"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/L0"}}}}}}},"components":{"schemas":{` + schemas + `}}}`))
	if err != nil {
		t.Fatal(err)
	}
	var node any = doc.Operations[0].Body.Schema
	for i := 0; i <= 5; i++ {
		node = node.(map[string]any)["properties"].(map[string]any)["next"].(map[string]any)["anyOf"].([]any)[0]
	}
	leaf := node.(map[string]any)["properties"].(map[string]any)["leaf"].(map[string]any)
	if leaf["type"] != "string" || len(leaf["enum"].([]any)) != 2 {
		t.Fatalf("leaf = %v", leaf)
	}
}

func TestFanOutIsBounded(t *testing.T) {
	// Each level references the next twice: 2^30 nodes if expanded naively.
	schemas := `"S30":{"type":"string"}`
	for i := 29; i >= 0; i-- {
		schemas += fmt.Sprintf(`,"S%d":{"type":"object","properties":{"l":{"$ref":"#/components/schemas/S%d"},"r":{"$ref":"#/components/schemas/S%d"}}}`, i, i+1, i+1)
	}
	doc, err := spec.Parse([]byte(`{"openapi":"3.1.0","paths":{"/bomb":{"post":{"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/S0"}}}}}},"/ok":{"get":{}}},"components":{"schemas":{` + schemas + `}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Operations) != 1 || doc.Operations[0].Path != "/ok" {
		t.Fatalf("operations = %+v", doc.Operations)
	}
	if len(doc.Skipped) != 1 || !strings.Contains(doc.Skipped[0].Reason, "too large") {
		t.Fatalf("skipped = %+v", doc.Skipped)
	}
}

func TestPathItemRefsUseTheOperationBudget(t *testing.T) {
	schemas := `"S30":{"type":"string"}`
	for i := 29; i >= 0; i-- {
		schemas += fmt.Sprintf(`,"S%d":{"type":"object","properties":{"l":{"$ref":"#/components/schemas/S%d"},"r":{"$ref":"#/components/schemas/S%d"}}}`, i, i+1, i+1)
	}
	doc, err := spec.Parse([]byte(`{"openapi":"3.1.0","paths":{"/x":{"$ref":"#/components/pathItems/P"},"/y":{"$ref":"#/components/pathItems/Q"}},"components":{"pathItems":{"P":{"post":{"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/S0"}}}}}},"Q":{"get":{"operationId":"q"}}},"schemas":{` + schemas + `}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Operations) != 1 || doc.Operations[0].ID != "q" {
		t.Fatalf("operations = %+v", doc.Operations)
	}
	if len(doc.Skipped) != 1 || !strings.Contains(doc.Skipped[0].Reason, "too large") {
		t.Fatalf("skipped = %+v", doc.Skipped)
	}
}

func TestObjectQueryParametersAreSkipped(t *testing.T) {
	doc, err := spec.Parse([]byte(`{"openapi":"3.1.0","paths":{"/a":{"get":{"parameters":[{"name":"filter","in":"query","schema":{"type":"object"}}]}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Operations) != 0 || len(doc.Skipped) != 1 {
		t.Fatalf("operations = %+v skipped = %+v", doc.Operations, doc.Skipped)
	}
}

func TestPathsMustStayOnTheHost(t *testing.T) {
	doc, err := spec.Parse([]byte(`{"openapi":"3.1.0","paths":{"@evil.example/x":{"get":{}},"//evil.example/y":{"get":{}},"/a?b=1":{"get":{}},"/fine":{"get":{}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Operations) != 1 || doc.Operations[0].Path != "/fine" || len(doc.Skipped) != 3 {
		t.Fatalf("operations = %+v skipped = %+v", doc.Operations, doc.Skipped)
	}
}

func TestUnsupportedQueryStylesAreSkipped(t *testing.T) {
	doc, err := spec.Parse([]byte(`{"openapi":"3.1.0","paths":{"/a":{"get":{"parameters":[{"name":"f","in":"query","style":"deepObject","schema":{"type":"object"}}]}},"/b":{"get":{"parameters":[{"name":"ids","in":"query","style":"pipeDelimited","schema":{"type":"array"}}]}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Operations) != 0 || len(doc.Skipped) != 2 {
		t.Fatalf("operations = %+v skipped = %+v", doc.Operations, doc.Skipped)
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
