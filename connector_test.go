package connectors_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omniaura/connectors"
	"github.com/omniaura/connectors/spec"
)

const fixtureKey = "pk_anon_fixture_0123456789"

func fixtureDoc(t *testing.T) *spec.Document {
	t.Helper()
	data, err := os.ReadFile("testdata/stays.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := spec.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func manifest(auth connectors.Auth) connectors.Manifest {
	return connectors.Manifest{
		Slug:  "stays",
		Name:  "Stays",
		Spec:  "testdata/stays.json",
		Auth:  auth,
		Check: &connectors.Check{Path: "/v1/keys/{api_key}/stats"},
		Tools: connectors.ToolFilter{Exclude: []string{"health", "create_key"}},
	}
}

// bearerOnly answers like Blue Pillow: only "Authorization: Bearer <key>"
// authenticates.
type recorder struct {
	calls   atomic.Int32
	lastReq *http.Request
	body    string
}

func bearerOnly(t *testing.T, rec *recorder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		rec.body = string(b)
		rec.lastReq = r
		if r.Header.Get("Authorization") != "Bearer "+fixtureKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"authentication_failed","message":"No API key provided."}}`)
			return
		}
		switch {
		case r.URL.Path == "/v1/keys/"+fixtureKey+"/stats":
			_, _ = io.WriteString(w, `{"key":"redacted","requests_today":1}`)
		case strings.HasPrefix(r.URL.Path, "/v1/properties/"):
			if strings.TrimPrefix(r.URL.Path, "/v1/properties/") == "missing" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":{"code":"not_found"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"`+strings.TrimPrefix(r.URL.EscapedPath(), "/v1/properties/")+`"}`)
		case r.URL.Path == "/v1/search/stays":
			_, _ = io.WriteString(w, `{"results":[]}`)
		case r.URL.Path == "/v1/tree":
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newConnector(t *testing.T, auth connectors.Auth, srv *httptest.Server) *connectors.Connector {
	t.Helper()
	m := manifest(auth)
	if auth.Type == connectors.AuthBasic || auth.Type == connectors.AuthNone {
		m.Check = nil // the fixture check path needs an api_key field
	}
	c, err := connectors.New(m, fixtureDoc(t), connectors.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestToolsFromFastAPIDocument(t *testing.T) {
	c := newConnector(t, connectors.Auth{Type: connectors.AuthBearer}, bearerOnly(t, &recorder{}))
	var names []string
	for _, tool := range c.Tools() {
		names = append(names, tool.Name)
	}
	want := []string{"STAYS_KEY_STATS", "STAYS_GET_PROPERTY", "STAYS_SEARCH_STAYS", "STAYS_TREE"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	skipped := map[string]string{}
	for _, s := range c.Skipped() {
		skipped[s.Path] = s.Reason
	}
	if !strings.Contains(skipped["/v1/upload"], "no JSON media type") || skipped["/v1/old"] != "deprecated" {
		t.Fatalf("skipped = %v", skipped)
	}

	get, _ := c.Tool("STAYS_GET_PROPERTY")
	if !get.ReadOnly || get.Description != "Get Property. Get canonical details for one property." {
		t.Fatalf("get property = %+v", get)
	}
	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
		Additional bool                      `json:"additionalProperties"`
	}
	if err := json.Unmarshal(get.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	if _, leaked := schema.Properties["authorization"]; leaked {
		t.Fatal("the spec's authorization header must not be a model argument")
	}
	if _, leaked := schema.Properties["session"]; leaked {
		t.Fatal("cookie parameters must be dropped")
	}
	lang := schema.Properties["language"]
	if lang["type"] != "string" || lang["example"] != nil || lang["x-internal"] != nil {
		t.Fatalf("language schema not resolved and cleaned: %v", lang)
	}
	if strings.Join(schema.Required, ",") != "property_id" || schema.Additional {
		t.Fatalf("required = %v additional = %v", schema.Required, schema.Additional)
	}

	search, _ := c.Tool("STAYS_SEARCH_STAYS")
	if search.ReadOnly {
		t.Fatal("POST must not be read-only")
	}
	schema.Properties, schema.Required = nil, nil
	if err := json.Unmarshal(search.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema.Properties["query"]; !ok {
		t.Fatalf("body properties should be flattened: %s", search.Schema)
	}
	if _, ok := schema.Properties["request_id"]; ok {
		t.Fatal("readOnly body properties must be dropped")
	}
	if strings.Join(schema.Required, ",") != "query" {
		t.Fatalf("required = %v (readOnly request_id must leave required too)", schema.Required)
	}
	if typ, _ := schema.Properties["currency"]["type"].([]any); len(typ) != 2 || typ[1] != "null" {
		t.Fatalf("nullable not folded into type: %v", schema.Properties["currency"])
	}
	if _, ok := schema.Properties["Idempotency-Key"]; !ok {
		t.Fatal("non-auth header parameters stay available")
	}

	tree, _ := c.Tool("STAYS_TREE")
	if !strings.Contains(string(tree.Schema), `"body"`) || !strings.Contains(string(tree.Schema), "Recursive Node") {
		t.Fatalf("open object bodies nest under body and recursion is cut: %s", tree.Schema)
	}
}

func TestExecuteSendsBearerAndBuildsRequest(t *testing.T) {
	rec := &recorder{}
	c := newConnector(t, connectors.Auth{Type: connectors.AuthBearer}, bearerOnly(t, rec))
	ctx := context.Background()

	// A pasted "Bearer " prefix is not doubled.
	for _, key := range []string{fixtureKey, "Bearer " + fixtureKey, "  bearer " + fixtureKey + " "} {
		r, err := c.Execute(ctx, connectors.Credentials{"api_key": key}, "STAYS_GET_PROPERTY",
			json.RawMessage(`{"property_id":"prop 1/2","include":["reviews","photos"],"language":"it"}`))
		if err != nil {
			t.Fatal(err)
		}
		if !r.OK() {
			t.Fatalf("key %q: status %d body %s", key, r.Status, r.Body)
		}
	}
	if got := rec.lastReq.URL.EscapedPath(); got != "/v1/properties/prop%201%2F2" {
		t.Fatalf("path = %s", got)
	}
	if got := rec.lastReq.URL.Query(); got.Get("include") != "reviews,photos" || got.Get("language") != "it" {
		t.Fatalf("query = %v", got)
	}

	r, err := c.Execute(ctx, connectors.Credentials{"api_key": fixtureKey}, "STAYS_SEARCH_STAYS",
		json.RawMessage(`{"query":"Paris","guests":{"adults":2},"Idempotency-Key":"abc"}`))
	if err != nil || !r.OK() {
		t.Fatalf("search: %v %+v", err, r)
	}
	if rec.body != `{"guests":{"adults":2},"query":"Paris"}` {
		t.Fatalf("body = %s", rec.body)
	}
	if rec.lastReq.Header.Get("Idempotency-Key") != "abc" || rec.lastReq.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", rec.lastReq.Header)
	}
}

func TestExecuteReturnsAppErrorsAsResults(t *testing.T) {
	c := newConnector(t, connectors.Auth{Type: connectors.AuthBearer}, bearerOnly(t, &recorder{}))
	ctx := context.Background()
	r, err := c.Execute(ctx, connectors.Credentials{"api_key": fixtureKey}, "STAYS_GET_PROPERTY", json.RawMessage(`{"property_id":"missing"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != http.StatusNotFound || r.String() != `{"error":{"error":{"code":"not_found"}},"status":404}` {
		t.Fatalf("result = %d %s", r.Status, r.String())
	}
	r, err = c.Execute(ctx, connectors.Credentials{"api_key": "pk_anon_wrong"}, "STAYS_GET_PROPERTY", json.RawMessage(`{"property_id":"x"}`))
	if err != nil || !r.CredentialsRejected() {
		t.Fatalf("wrong key: %v %+v", err, r)
	}
}

func TestExecuteRejectsBadArguments(t *testing.T) {
	c := newConnector(t, connectors.Auth{Type: connectors.AuthBearer}, bearerOnly(t, &recorder{}))
	ctx := context.Background()
	creds := connectors.Credentials{"api_key": fixtureKey}
	cases := map[string]struct {
		tool, args string
		want       func(error) bool
	}{
		"unknown tool":     {"STAYS_NOPE", `{}`, connectors.ErrUnknownTool.Is},
		"unknown argument": {"STAYS_GET_PROPERTY", `{"property_id":"x","evil":1}`, connectors.ErrInvalidArguments.Is},
		"missing required": {"STAYS_GET_PROPERTY", `{}`, connectors.ErrInvalidArguments.Is},
		"null required":    {"STAYS_GET_PROPERTY", `{"property_id":null}`, connectors.ErrInvalidArguments.Is},
		"dot-dot path":     {"STAYS_GET_PROPERTY", `{"property_id":".."}`, connectors.ErrInvalidArguments.Is},
		"object path":      {"STAYS_GET_PROPERTY", `{"property_id":{"a":1}}`, connectors.ErrInvalidArguments.Is},
		"header injection": {"STAYS_SEARCH_STAYS", `{"query":"x","Idempotency-Key":"a\r\nX-Evil: 1"}`, connectors.ErrInvalidArguments.Is},
		"not an object":    {"STAYS_GET_PROPERTY", `[1]`, connectors.ErrInvalidArguments.Is},
		"no credentials":   {"STAYS_GET_PROPERTY", `{"property_id":"x"}`, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			use := creds
			want := tc.want
			if want == nil {
				use, want = connectors.Credentials{}, connectors.ErrMissingCredentials.Is
			}
			_, err := c.Execute(ctx, use, tc.tool, json.RawMessage(tc.args))
			if !want(err) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestCheck(t *testing.T) {
	ctx := context.Background()
	srv := bearerOnly(t, &recorder{})
	bearer := newConnector(t, connectors.Auth{Type: connectors.AuthBearer}, srv)
	if err := bearer.Check(ctx, connectors.Credentials{"api_key": fixtureKey}); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	if err := bearer.Check(ctx, connectors.Credentials{"api_key": "Bearer " + fixtureKey}); err != nil {
		t.Fatalf("pasted Bearer prefix rejected: %v", err)
	}
	if err := bearer.Check(ctx, connectors.Credentials{"api_key": "pk_anon_wrong"}); !connectors.ErrCredentialsRejected.Is(err) {
		t.Fatalf("wrong key: %v", err)
	}
	if err := bearer.Check(ctx, connectors.Credentials{}); !connectors.ErrMissingCredentials.Is(err) {
		t.Fatalf("no key: %v", err)
	}

	// Negative control for DITTO-2340: the same valid key sent as a bare
	// Authorization value (what the hosted Composio toolkit appears to do)
	// is rejected. If this passed, the bearer case above would prove nothing.
	bare := newConnector(t, connectors.Auth{Type: connectors.AuthHeader, Name: "Authorization"}, srv)
	if err := bare.Check(ctx, connectors.Credentials{"api_key": fixtureKey}); !connectors.ErrCredentialsRejected.Is(err) {
		t.Fatalf("bare Authorization should be rejected, got %v", err)
	}

	// An endpoint that answers an unknown key with 404 can opt in.
	m404 := manifest(connectors.Auth{Type: connectors.AuthBearer})
	m404.Check = &connectors.Check{Path: "/v1/properties/missing", RejectedStatus: []int{401, 404}}
	c404, err := connectors.New(m404, fixtureDoc(t), connectors.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := c404.Check(ctx, connectors.Credentials{"api_key": fixtureKey}); !connectors.ErrCredentialsRejected.Is(err) {
		t.Fatalf("404 listed in rejected_status: %v", err)
	}
	if err := bearer.Check(ctx, connectors.Credentials{"api_key": fixtureKey + "x"}); !connectors.ErrCredentialsRejected.Is(err) {
		t.Fatalf("default rejected statuses: %v", err)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer down.Close()
	if err := newConnector(t, connectors.Auth{Type: connectors.AuthBearer}, down).Check(ctx, connectors.Credentials{"api_key": fixtureKey}); !connectors.ErrCheckUnavailable.Is(err) {
		t.Fatalf("5xx should be unavailable, not rejected: %v", err)
	}
	m := manifest(connectors.Auth{Type: connectors.AuthBearer})
	m.Check = nil
	c, err := connectors.New(m, fixtureDoc(t), connectors.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Check(ctx, connectors.Credentials{"api_key": fixtureKey}); !connectors.ErrNoCheck.Is(err) {
		t.Fatalf("no check: %v", err)
	}
}

func TestAuthShapes(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	ctx := context.Background()
	args := json.RawMessage(`{"property_id":"p"}`)

	c := newConnector(t, connectors.Auth{Type: connectors.AuthHeader, Name: "X-Api-Key", Prefix: "Token "}, srv)
	if _, err := c.Execute(ctx, connectors.Credentials{"api_key": "Token k1"}, "STAYS_GET_PROPERTY", args); err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("X-Api-Key") != "Token k1" || got.Header.Get("Authorization") != "" {
		t.Fatalf("header auth: %v", got.Header)
	}

	c = newConnector(t, connectors.Auth{Type: connectors.AuthQuery, Name: "key"}, srv)
	if _, err := c.Execute(ctx, connectors.Credentials{"api_key": "k2"}, "STAYS_GET_PROPERTY", args); err != nil {
		t.Fatal(err)
	}
	if got.URL.Query().Get("key") != "k2" {
		t.Fatalf("query auth: %v", got.URL)
	}

	c = newConnector(t, connectors.Auth{Type: connectors.AuthBasic}, srv)
	if _, err := c.Execute(ctx, connectors.Credentials{"username": "u", "password": "p"}, "STAYS_GET_PROPERTY", args); err != nil {
		t.Fatal(err)
	}
	if u, p, ok := got.BasicAuth(); !ok || u != "u" || p != "p" {
		t.Fatalf("basic auth: %v", got.Header)
	}
}

func TestWritesAreNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := newConnector(t, connectors.Auth{Type: connectors.AuthBearer}, srv)
	r, err := c.Execute(context.Background(), connectors.Credentials{"api_key": "k"}, "STAYS_SEARCH_STAYS", json.RawMessage(`{"query":"x"}`))
	if err != nil || r.Status != http.StatusServiceUnavailable {
		t.Fatalf("%v %+v", err, r)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("a failed POST was sent %d times", n)
	}
}

func TestCredentialsDoNotFollowCrossHostRedirects(t *testing.T) {
	var leaked atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization") + "|" + r.URL.RawQuery)
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer srv.Close()
	for _, auth := range []connectors.Auth{{Type: connectors.AuthBearer}, {Type: connectors.AuthQuery, Name: "key"}} {
		c := newConnector(t, auth, srv)
		r, err := c.Execute(context.Background(), connectors.Credentials{"api_key": "secret"}, "STAYS_GET_PROPERTY", json.RawMessage(`{"property_id":"p"}`))
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != http.StatusFound {
			t.Fatalf("redirect should be returned, not followed: %d", r.Status)
		}
	}
	if v := leaked.Load(); v != nil {
		t.Fatalf("request reached the other host: %v", v)
	}
}

func TestLargeResponsesAreTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `"`+strings.Repeat("a", 5000)+`"`)
	}))
	defer srv.Close()
	m := manifest(connectors.Auth{Type: connectors.AuthNone})
	m.Check = nil
	c, err := connectors.New(m, fixtureDoc(t),
		connectors.WithBaseURL(srv.URL), connectors.WithMaxResponseBytes(1000))
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Execute(context.Background(), nil, "STAYS_GET_PROPERTY", json.RawMessage(`{"property_id":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || len(r.Body) != 1000 || !strings.HasSuffix(r.String(), "[response truncated]") {
		t.Fatalf("truncated=%v len=%d", r.Truncated, len(r.Body))
	}
}

func TestManifestValidation(t *testing.T) {
	bad := []string{
		"slug: Bad\nspec: x\nauth: {type: bearer}\n",
		"slug: ok\nauth: {type: bearer}\n",
		"slug: ok\nspec: x\nauth: {type: header}\n",
		"slug: ok\nspec: x\nauth: {type: oauth2}\n",
		"slug: ok\nspec: x\nbase_url: http://example.com\nauth: {type: bearer}\n",
		"slug: ok\nspec: x\nauth: {type: bearer}\ncheck: {path: /v1/{nope}}\n",
		"slug: ok\nspec: x\nauth: {type: bearer}\nunknown_key: 1\n",
		"slug: ok\nspec: x\nauth: {type: bearer}\ncheck: {path: /x, rejected_status: [500]}\n",
	}
	for _, src := range bad {
		if _, err := connectors.ParseManifest([]byte(src)); err == nil {
			t.Errorf("accepted invalid manifest:\n%s", src)
		}
	}
	m, err := connectors.ParseManifest([]byte("slug: ok\nspec: x\nauth: {type: basic}\n"))
	if err != nil || len(m.Auth.Fields) != 2 {
		t.Fatalf("basic defaults: %v %+v", err, m.Auth)
	}
}

func TestIncludeMustMatch(t *testing.T) {
	m := manifest(connectors.Auth{Type: connectors.AuthBearer})
	m.Tools = connectors.ToolFilter{Include: []string{"search_stays", "typo_op"}}
	if _, err := connectors.New(m, fixtureDoc(t), connectors.WithBaseURL("https://example.com")); err == nil {
		t.Fatal("an include entry that matches nothing must fail loudly")
	}
}
