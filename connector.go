// Package connectors turns an OpenAPI document plus a small manifest into
// tools an AI agent can call: each operation becomes a named tool with a
// JSON Schema for its arguments, and Execute sends the call to the app with
// the person's credential attached the way the manifest says.
package connectors

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omniaura/connectors/spec"
	"github.com/omniaura/go-kit/errs"
	"github.com/omniaura/go-kit/net/hit"
)

const (
	// DefaultTimeout bounds one request to the app.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxResponseBytes bounds how much of a response body is kept.
	DefaultMaxResponseBytes = 256 << 10
	// DefaultMaxSpecBytes bounds an OpenAPI document.
	DefaultMaxSpecBytes = 16 << 20
	maxDescription      = 1024
	maxToolName         = 64
	// upstreamBodyCap mirrors maxUpstreamBody in go-kit net/hit (errors.go,
	// be13686): how much of a non-2xx body hit keeps before appending "…".
	upstreamBodyCap = 2048
)

// Credentials are a person's values for the manifest's auth fields.
type Credentials map[string]string

// Tool is one callable operation.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"input_schema"`
	OperationID string          `json:"operation_id,omitempty"`
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	// ReadOnly is true for GET and HEAD. Hosts that gate writes behind an
	// approval should gate every tool that is not ReadOnly.
	ReadOnly bool `json:"read_only"`

	inputs   map[string]input
	bodyFlat bool
	hasBody  bool
}

type input struct {
	in, name string // in: path | query | header | body (flattened property) | bodyAll
	explode  bool
}

// Result is the app's HTTP answer to a tool call. Non-2xx answers are
// results too: the model should see "404 property not found".
//
// Every success is reported as 200 (the transport does not expose 201 or
// 204), and a non-2xx body is kept up to 2 KiB, not the full response cap.
type Result struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type,omitempty"`
	Body        []byte `json:"-"`
	Truncated   bool   `json:"truncated,omitempty"`
}

// OK reports a 2xx status.
func (r Result) OK() bool { return r.Status >= 200 && r.Status < 300 }

// CredentialsRejected reports a 401 or 403, which a host should surface as
// "reconnect this app" rather than as a tool failure.
func (r Result) CredentialsRejected() bool {
	return r.Status == http.StatusUnauthorized || r.Status == http.StatusForbidden
}

// String renders the result for a model: the body for a 2xx, otherwise a
// small JSON object with the status and the app's error body.
func (r Result) String() string {
	body := strings.TrimSpace(string(r.Body))
	if r.Truncated {
		body += "\n[response truncated]"
	}
	if r.OK() {
		if body == "" {
			return fmt.Sprintf(`{"status":%d}`, r.Status)
		}
		return body
	}
	out := map[string]any{"status": r.Status}
	if json.Valid(r.Body) && !r.Truncated {
		out["error"] = json.RawMessage(r.Body)
	} else if body != "" {
		out["error"] = body
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// Connector is an app's tools bound to its base URL. It holds no
// credentials; every call takes them, so one Connector serves every person.
type Connector struct {
	manifest Manifest
	base     *url.URL
	tools    []Tool
	byName   map[string]int
	skipped  []spec.Skip
	client   *http.Client
	maxBytes int64
	timeout  time.Duration
}

type options struct {
	client   *http.Client
	baseURL  string
	specDir  string
	maxBytes int64
	timeout  time.Duration
}

// Option configures a Connector.
type Option func(*options)

// WithHTTPClient sets the client used for app requests. Its CheckRedirect is
// replaced: credentials must never follow a redirect to another host.
func WithHTTPClient(c *http.Client) Option { return func(o *options) { o.client = c } }

// WithBaseURL overrides the manifest and document base URL (tests, proxies).
func WithBaseURL(u string) Option { return func(o *options) { o.baseURL = u } }

// WithSpecDir resolves a relative manifest spec path against dir.
func WithSpecDir(dir string) Option { return func(o *options) { o.specDir = dir } }

// WithMaxResponseBytes bounds a kept response body.
func WithMaxResponseBytes(n int64) Option { return func(o *options) { o.maxBytes = n } }

// WithTimeout bounds one app request.
func WithTimeout(d time.Duration) Option { return func(o *options) { o.timeout = d } }

func collect(opts []Option) options {
	o := options{maxBytes: DefaultMaxResponseBytes, timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// Load fetches the manifest's OpenAPI document and builds the connector.
func Load(ctx context.Context, m Manifest, opts ...Option) (*Connector, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	o := collect(opts)
	data, err := fetchSpec(ctx, m.Spec, o)
	if err != nil {
		return nil, ErrSpecUnavailable.New(ctx).AddError(err).AddAttrs(errs.String("connector", m.Slug))
	}
	doc, err := spec.Parse(data)
	if err != nil {
		return nil, ErrSpecUnavailable.New(ctx).AddError(err).AddAttrs(errs.String("connector", m.Slug))
	}
	return New(m, doc, opts...)
}

func fetchSpec(ctx context.Context, ref string, o options) ([]byte, error) {
	if strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "http://") {
		u, err := url.Parse(ref)
		if err != nil {
			return nil, err
		}
		if !allowedScheme(u) {
			return nil, fmt.Errorf("spec URL must use https (http only on loopback)")
		}
		var out []byte
		err = hit.GET[hit.AnyError](ref).
			Client(limitedClient(o.client, DefaultMaxSpecBytes)).
			Timeout(o.timeout).
			Header("Accept", "application/json, application/yaml;q=0.9, */*;q=0.5").
			ResponseAs(hit.Bytes).
			Do(ctx, &out)
		if err != nil {
			return nil, err
		}
		if int64(len(out)) > DefaultMaxSpecBytes {
			return nil, fmt.Errorf("spec is larger than %d bytes", DefaultMaxSpecBytes)
		}
		return out, nil
	}
	path := ref
	if !filepath.IsAbs(path) && o.specDir != "" {
		path = filepath.Join(o.specDir, path)
	}
	return os.ReadFile(path)
}

// New builds a connector from a manifest and an already parsed document.
func New(m Manifest, doc *spec.Document, opts ...Option) (*Connector, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	o := collect(opts)
	baseURL := o.baseURL
	if baseURL == "" {
		baseURL = m.BaseURL
	}
	if baseURL == "" && len(doc.Servers) > 0 {
		baseURL = doc.Servers[0]
	}
	if baseURL == "" {
		return nil, fmt.Errorf("connector %s: no base URL in the manifest or the document", m.Slug)
	}
	if err := checkBaseURL(baseURL); err != nil {
		return nil, fmt.Errorf("connector %s: %w", m.Slug, err)
	}
	base, _ := url.Parse(strings.TrimRight(baseURL, "/"))
	c := &Connector{
		manifest: m,
		base:     base,
		byName:   map[string]int{},
		skipped:  slices.Clone(doc.Skipped),
		client:   limitedClient(o.client, o.maxBytes),
		maxBytes: o.maxBytes,
		timeout:  o.timeout,
	}
	allowWrites := m.Tools.AllowWrites == nil || *m.Tools.AllowWrites
	for _, op := range doc.Operations {
		name := toolName(m, op)
		if !selected(m.Tools, op, name) {
			continue
		}
		if !allowWrites && op.Method != http.MethodGet && op.Method != http.MethodHead {
			continue
		}
		if op.Deprecated && len(m.Tools.Include) == 0 {
			c.skipped = append(c.skipped, spec.Skip{Method: op.Method, Path: op.Path, OperationID: op.ID, Reason: "deprecated"})
			continue
		}
		tool, err := c.buildTool(name, op)
		if err != nil {
			c.skipped = append(c.skipped, spec.Skip{Method: op.Method, Path: op.Path, OperationID: op.ID, Reason: err.Error()})
			continue
		}
		if _, dup := c.byName[tool.Name]; dup {
			return nil, fmt.Errorf("connector %s: two operations map to tool %s; rename one with tools.names", m.Slug, tool.Name)
		}
		c.byName[tool.Name] = len(c.tools)
		c.tools = append(c.tools, tool)
	}
	for _, want := range m.Tools.Include {
		if !c.offers(want) {
			return nil, fmt.Errorf("connector %s: tools.include names %q, which the document does not offer", m.Slug, want)
		}
	}
	// An exclude that matches nothing usually means the document renamed
	// the operation, and the tool it was meant to hide is now offered.
	for _, drop := range m.Tools.Exclude {
		found := false
		for _, op := range doc.Operations {
			if drop == op.ID || drop == op.ShortID() || drop == toolName(m, op) {
				found = true
				break
			}
		}
		for _, s := range doc.Skipped {
			short := spec.Operation{ID: s.OperationID, Method: s.Method, Path: s.Path}.ShortID()
			if s.OperationID != "" && (drop == s.OperationID || drop == short) {
				found = true // skipped anyway, so still not offered
			}
		}
		if !found {
			return nil, fmt.Errorf("connector %s: tools.exclude names %q, which the document does not offer", m.Slug, drop)
		}
	}
	return c, nil
}

func (c *Connector) offers(want string) bool {
	for _, t := range c.tools {
		if t.Name == want || t.OperationID == want || shortOf(t) == want {
			return true
		}
	}
	return false
}

func shortOf(t Tool) string {
	return spec.Operation{ID: t.OperationID, Method: t.Method, Path: t.Path}.ShortID()
}

func selected(f ToolFilter, op spec.Operation, name string) bool {
	match := func(list []string) bool {
		for _, s := range list {
			if s == op.ID || s == op.ShortID() || s == name {
				return true
			}
		}
		return false
	}
	if len(f.Include) > 0 && !match(f.Include) {
		return false
	}
	return !match(f.Exclude)
}

var nonName = regexp.MustCompile(`[^A-Za-z0-9]+`)

func toolName(m Manifest, op spec.Operation) string {
	base := ""
	for _, key := range []string{op.ShortID(), op.ID} { // the full ID wins
		if n, ok := m.Tools.Names[key]; ok && key != "" {
			base = n
		}
	}
	if base == "" {
		base = op.ShortID()
	}
	if base == "" {
		base = strings.ToLower(op.Method) + " " + op.Path
	}
	base = camelToSnake(base)
	name := strings.Trim(nonName.ReplaceAllString(strings.ToUpper(m.Slug+"_"+base), "_"), "_")
	if len(name) > maxToolName {
		name = strings.TrimRight(name[:maxToolName], "_")
	}
	return name
}

var camel = regexp.MustCompile(`([a-z0-9])([A-Z])`)

func camelToSnake(s string) string { return camel.ReplaceAllString(s, "${1}_${2}") }

// reserved headers are set by the connector, never by the model.
var reserved = map[string]bool{
	"authorization": true, "proxy-authorization": true, "content-type": true,
	"accept": true, "content-length": true, "transfer-encoding": true,
	"connection": true, "te": true, "upgrade": true, "trailer": true,
	"host": true, "cookie": true, "forwarded": true,
}

func reservedHeader(name string) bool {
	n := strings.ToLower(name)
	return reserved[n] || strings.HasPrefix(n, "x-forwarded-")
}

func (c *Connector) isAuthParam(p spec.Parameter) bool {
	a := c.manifest.Auth
	switch p.In {
	case "header":
		if reservedHeader(p.Name) {
			return true
		}
		return a.Type == AuthHeader && strings.EqualFold(p.Name, a.Name)
	case "query":
		return a.Type == AuthQuery && p.Name == a.Name
	}
	return false
}

func (c *Connector) buildTool(name string, op spec.Operation) (Tool, error) {
	t := Tool{
		Name:        name,
		OperationID: op.ID,
		Method:      op.Method,
		Path:        op.Path,
		ReadOnly:    op.Method == http.MethodGet || op.Method == http.MethodHead,
		inputs:      map[string]input{},
	}
	props := map[string]any{}
	var required []string
	addProp := func(key string, schema map[string]any, desc string, in input, req bool) error {
		if _, dup := props[key]; dup {
			return fmt.Errorf("argument %q is declared twice", key)
		}
		s := make(map[string]any, len(schema)+1)
		for k, v := range schema {
			s[k] = v
		}
		if desc != "" {
			if _, has := s["description"]; !has {
				s["description"] = desc
			}
		}
		props[key] = s
		t.inputs[key] = in
		if req {
			required = append(required, key)
		}
		return nil
	}
	for _, p := range op.Parameters {
		if c.isAuthParam(p) {
			continue
		}
		explode := p.Explode == nil || *p.Explode // form style explodes by default
		if p.Style != "" && p.Style != "form" && p.In == "query" {
			explode = p.Explode != nil && *p.Explode
		}
		key := p.Name
		if _, dup := props[key]; dup {
			key = p.In + "_" + p.Name
		}
		if err := addProp(key, p.Schema, p.Description, input{in: p.In, name: p.Name, explode: explode}, p.Required); err != nil {
			return Tool{}, err
		}
	}
	if op.Body != nil {
		t.hasBody = true
		bodyProps, flat := flattenable(op.Body.Schema, props)
		if flat {
			t.bodyFlat = true
			req := map[string]bool{}
			if op.Body.Required {
				for _, r := range asStrings(op.Body.Schema["required"]) {
					req[r] = true
				}
			}
			keys := make([]string, 0, len(bodyProps))
			for k := range bodyProps {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				schema, _ := bodyProps[k].(map[string]any)
				if err := addProp(k, schema, "", input{in: "body", name: k}, req[k]); err != nil {
					return Tool{}, err
				}
			}
		} else {
			key := "body"
			if _, dup := props[key]; dup {
				key = "request_body"
			}
			if err := addProp(key, op.Body.Schema, op.Body.Description, input{in: "bodyAll"}, op.Body.Required); err != nil {
				return Tool{}, err
			}
		}
	}
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		sort.Strings(required)
		schema["required"] = required
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return Tool{}, fmt.Errorf("schema does not encode: %w", err)
	}
	t.Schema = raw
	t.Description = describe(op)
	return t, nil
}

// flattenable reports whether a body's properties can sit next to the
// parameters as top-level arguments, which models fill in more reliably.
func flattenable(s map[string]any, taken map[string]any) (map[string]any, bool) {
	if s == nil {
		return nil, false
	}
	if t, _ := s["type"].(string); t != "object" {
		return nil, false
	}
	for _, k := range []string{"allOf", "oneOf", "anyOf", "not", "if", "patternProperties"} {
		if _, ok := s[k]; ok {
			return nil, false
		}
	}
	if ap, ok := s["additionalProperties"]; ok && ap != false {
		return nil, false
	}
	props, _ := s["properties"].(map[string]any)
	if len(props) == 0 {
		return nil, false
	}
	for k := range props {
		if _, clash := taken[k]; clash {
			return nil, false
		}
	}
	return props, true
}

func describe(op spec.Operation) string {
	parts := []string{}
	if s := strings.TrimSpace(op.Summary); s != "" {
		parts = append(parts, strings.TrimRight(s, ".")+".")
	}
	if d := strings.TrimSpace(op.Description); d != "" && d != strings.TrimSpace(op.Summary) {
		parts = append(parts, d)
	}
	if len(parts) == 0 {
		parts = append(parts, op.Method+" "+op.Path)
	}
	out := strings.Join(parts, " ")
	if len(out) > maxDescription {
		cut := maxDescription - len("…")
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut-- // never split a multi-byte rune
		}
		out = strings.TrimSpace(out[:cut]) + "…"
	}
	return out
}

func asStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Manifest returns the connector's manifest, with defaults filled.
func (c *Connector) Manifest() Manifest { return c.manifest }

// Tools returns the callable tools in document order.
func (c *Connector) Tools() []Tool { return slices.Clone(c.tools) }

// Tool returns one tool by name.
func (c *Connector) Tool(name string) (Tool, bool) {
	i, ok := c.byName[name]
	if !ok {
		return Tool{}, false
	}
	return c.tools[i], true
}

// Skipped lists operations that were left out, with the reason.
func (c *Connector) Skipped() []spec.Skip { return slices.Clone(c.skipped) }

// Execute runs one tool call. Arguments are the JSON object the model sent.
// Any HTTP answer is a Result, including 4xx and 5xx; the error is for calls
// that never got an answer or could not be built.
func (c *Connector) Execute(ctx context.Context, creds Credentials, name string, args json.RawMessage) (Result, error) {
	t, ok := c.Tool(name)
	if !ok {
		return Result{}, ErrUnknownTool.New(ctx).AddAttrs(errs.String("tool", name))
	}
	values := map[string]json.RawMessage{}
	if trimmed := bytes.TrimSpace(args); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if err := json.Unmarshal(trimmed, &values); err != nil {
			return Result{}, ErrInvalidArguments.New(ctx).AddError(err).Field("arguments", "must be a JSON object")
		}
	}
	for key := range values {
		if _, known := t.inputs[key]; !known {
			return Result{}, ErrInvalidArguments.New(ctx).Field(key, "is not an argument of this tool")
		}
	}
	var schema struct {
		Required []string `json:"required"`
	}
	_ = json.Unmarshal(t.Schema, &schema)
	for _, key := range schema.Required {
		if v, ok := values[key]; !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return Result{}, ErrInvalidArguments.New(ctx).Field(key, "is required")
		}
	}

	path := t.Path
	query := url.Values{}
	headers := http.Header{}
	bodyFields := map[string]json.RawMessage{}
	var body []byte
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		raw := values[key]
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		in := t.inputs[key]
		switch in.in {
		case "path":
			s, ok := scalar(raw)
			if !ok || s == "" || s == "." || s == ".." {
				return Result{}, ErrInvalidArguments.New(ctx).Field(key, "must be a non-empty string or number")
			}
			path = strings.ReplaceAll(path, "{"+in.name+"}", url.PathEscape(s))
		case "query":
			if err := addQuery(query, in, raw); err != nil {
				return Result{}, ErrInvalidArguments.New(ctx).Field(key, err.Error())
			}
		case "header":
			s, ok := scalar(raw)
			if !ok || strings.ContainsAny(s, "\r\n") {
				return Result{}, ErrInvalidArguments.New(ctx).Field(key, "must be a single-line string")
			}
			headers.Set(in.name, s)
		case "body":
			bodyFields[in.name] = raw
		case "bodyAll":
			body = raw
		}
	}
	if strings.Contains(path, "{") {
		return Result{}, ErrInvalidArguments.New(ctx).Field("path", "a path argument is missing")
	}
	if hasDotSegment(path) {
		return Result{}, ErrInvalidArguments.New(ctx).Field("path", "path arguments must not form . or .. segments")
	}
	if t.bodyFlat && (len(bodyFields) > 0 || t.Method == http.MethodPost || t.Method == http.MethodPut || t.Method == http.MethodPatch) {
		b, err := json.Marshal(bodyFields)
		if err != nil {
			return Result{}, ErrInvalidArguments.New(ctx).AddError(err)
		}
		body = b
	}
	if err := c.authorize(ctx, creds, headers, query); err != nil {
		return Result{}, err
	}
	return c.send(ctx, t.Method, path, t.Path, query, headers, body)
}

func hasDotSegment(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		dec, err := url.PathUnescape(seg)
		if err != nil {
			dec = seg
		}
		// An escaped "/" (%2F) hides a segment boundary from this split; a
		// server that decodes it before routing would see the dots.
		for _, part := range strings.Split(dec, "/") {
			if part == "." || part == ".." {
				return true
			}
		}
	}
	return false
}

func scalar(raw json.RawMessage) (string, bool) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		if t {
			return "true", true
		}
		return "false", true
	}
	return "", false
}

func addQuery(q url.Values, in input, raw json.RawMessage) error {
	if s, ok := scalar(raw); ok {
		q.Add(in.name, s)
		return nil
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		parts := make([]string, 0, len(list))
		for _, e := range list {
			s, ok := scalar(e)
			if !ok {
				return errors.New("must be a list of strings or numbers")
			}
			parts = append(parts, s)
		}
		if in.explode {
			for _, p := range parts {
				q.Add(in.name, p)
			}
		} else {
			q.Add(in.name, strings.Join(parts, ","))
		}
		return nil
	}
	if json.Valid(raw) {
		q.Add(in.name, string(bytes.TrimSpace(raw)))
		return nil
	}
	return errors.New("is not valid JSON")
}

func (c *Connector) authorize(ctx context.Context, creds Credentials, headers http.Header, query url.Values) error {
	a := c.manifest.Auth
	get := func(field string) (string, error) {
		v := strings.TrimSpace(creds[field])
		if v == "" {
			return "", ErrMissingCredentials.New(ctx).Field(field, "is required").AddAttrs(errs.String("connector", c.manifest.Slug))
		}
		if strings.ContainsAny(v, "\r\n") {
			return "", ErrMissingCredentials.New(ctx).Field(field, "must be a single line")
		}
		return v, nil
	}
	switch a.Type {
	case AuthNone:
		return nil
	case AuthBearer:
		key, err := get(a.keyField())
		if err != nil {
			return err
		}
		// People paste "Bearer pk_..." as often as "pk_...". Send one prefix.
		if len(key) > 7 && strings.EqualFold(key[:7], "bearer ") {
			key = strings.TrimSpace(key[7:])
		}
		headers.Set("Authorization", "Bearer "+key)
	case AuthHeader:
		key, err := get(a.keyField())
		if err != nil {
			return err
		}
		if a.Prefix != "" && strings.HasPrefix(strings.ToLower(key), strings.ToLower(a.Prefix)) {
			key = strings.TrimSpace(key[len(a.Prefix):])
		}
		headers.Set(a.Name, a.Prefix+key)
	case AuthQuery:
		key, err := get(a.keyField())
		if err != nil {
			return err
		}
		query.Set(a.Name, key)
	case AuthBasic:
		user, err := get("username")
		if err != nil {
			return err
		}
		pass, err := get("password")
		if err != nil {
			return err
		}
		headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	}
	return nil
}

// send issues one request. path is the filled-in path; logPath is its
// template, which is what reaches logs (a check path can hold a key).
func (c *Connector) send(ctx context.Context, method, path, logPath string, query url.Values, headers http.Header, body []byte) (Result, error) {
	full := c.base.String() + path
	if u, err := url.Parse(full); err != nil || u.Host != c.base.Host || u.Scheme != c.base.Scheme || u.User != nil {
		return Result{}, ErrInvalidArguments.New(ctx).Field("path", "does not stay on the app's host")
	}
	req := hit.GET[hit.AnyError](full).
		Method(method).
		Client(c.client).
		Timeout(c.timeout).
		Service(c.manifest.Slug).
		Coalesce(false).
		ResponseAs(hit.Bytes).
		Header("Accept", "application/json, */*;q=0.5")
	for k, vs := range query {
		for _, v := range vs {
			req = req.Query(k, v)
		}
	}
	for k := range headers {
		req = req.Header(k, headers.Get(k))
	}
	if body != nil {
		req = req.RawBody(body).Header("Content-Type", "application/json")
	}
	var out []byte
	err := req.Do(ctx, &out)
	if err == nil {
		return c.result(http.StatusOK, out), nil
	}
	var e *errs.Error
	if errors.As(err, &e) {
		if up, ok := e.UpstreamInfo(); ok && up.Status > 0 {
			// hit keeps the first upstreamBodyCap bytes and appends "…", so a
			// kept body of exactly that length is the only truncated shape.
			body, cut := up.Body, false
			if len(body) == upstreamBodyCap+len("…") && strings.HasSuffix(body, "…") {
				body, cut = body[:upstreamBodyCap], true
			}
			r := c.result(up.Status, []byte(body)) // may cap it further
			r.Truncated = r.Truncated || cut
			return r, nil
		}
	}
	// A transport error (*url.Error) carries the full request URL, which can
	// hold a key in the query or in a check path. Keep it as a private cause:
	// errors.Is still works, its text stays out of logs and records.
	return Result{}, ErrRequestFailed.New(ctx).AddPrivateError(err).AddAttrs(errs.String("connector", c.manifest.Slug), errs.String("method", method), errs.String("path", logPath))
}

// result keeps the status hit reported for errors; for 2xx hit does not
// expose the exact code, so 200 stands for every success.
func (c *Connector) result(status int, body []byte) Result {
	r := Result{Status: status, Body: body}
	if int64(len(body)) > c.maxBytes {
		r.Body = body[:c.maxBytes]
		r.Truncated = true
	}
	if json.Valid(r.Body) {
		r.ContentType = "application/json"
	}
	return r
}

// Check validates credentials with the manifest's check request. nil means
// the app accepted them.
func (c *Connector) Check(ctx context.Context, creds Credentials) error {
	chk := c.manifest.Check
	if chk == nil {
		return ErrNoCheck.New(ctx).AddAttrs(errs.String("connector", c.manifest.Slug))
	}
	path := chk.Path
	for _, name := range placeholders(chk.Path) {
		v := strings.TrimSpace(creds[name])
		if c.manifest.Auth.Type == AuthBearer && name == c.manifest.Auth.keyField() && len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
			v = strings.TrimSpace(v[7:])
		}
		if v == "" {
			return ErrMissingCredentials.New(ctx).Field(name, "is required")
		}
		path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(v))
	}
	if hasDotSegment(path) {
		return ErrMissingCredentials.New(ctx).Field("api_key", "is not a valid key")
	}
	headers := http.Header{}
	query := url.Values{}
	if err := c.authorize(ctx, creds, headers, query); err != nil {
		return err
	}
	r, err := c.send(ctx, chk.Method, path, chk.Path, query, headers, nil)
	if err != nil {
		return ErrCheckUnavailable.New(ctx).AddError(err)
	}
	switch {
	case r.OK():
		return nil
	case slices.Contains(chk.RejectedStatus, r.Status):
		return ErrCredentialsRejected.New(ctx).AddAttrs(errs.String("connector", c.manifest.Slug), errs.Int("status", r.Status))
	default:
		return ErrCheckUnavailable.New(ctx).AddAttrs(errs.String("connector", c.manifest.Slug), errs.Int("status", r.Status))
	}
}

// limitedClient copies base (or a default client), refuses redirects that
// leave the original host, and caps response bodies at max+1 bytes so an
// oversized answer is detectably truncated rather than read into memory.
func limitedClient(base *http.Client, max int64) *http.Client {
	c := &http.Client{}
	if base != nil {
		*c = *base
	}
	// One Connector serves every person: cookies one person's calls set
	// must not ride along on another's.
	c.Jar = nil
	rt := c.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	c.Transport = limitTransport{base: rt, max: max}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return http.ErrUseLastResponse
		}
		if req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return c
}

type limitTransport struct {
	base http.RoundTripper
	max  int64
}

func (t limitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rsp, err := t.base.RoundTrip(r)
	if err != nil || t.max <= 0 {
		return rsp, err
	}
	rsp.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(rsp.Body, t.max+1), rsp.Body}
	return rsp, nil
}
