// Package spec reads the parts of an OpenAPI 3.0 or 3.1 document that a
// connector needs: servers, operations, parameters and JSON request bodies,
// with local $refs resolved inline. It accepts JSON or YAML.
//
// It is deliberately not a full OpenAPI model. Anything it does not
// understand is skipped with a reason, never guessed at.
package spec

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// MaxRefDepth bounds how many $refs deep expansion follows. Deeper (or
// recursive) references are replaced by a schema that accepts any value.
const MaxRefDepth = 24

// MaxNodes bounds the total size of one operation after $ref expansion, so
// a document that references shared schemas many times cannot grow
// exponentially. An operation over the limit is skipped.
const MaxNodes = 200_000

// Document is a parsed OpenAPI document.
type Document struct {
	OpenAPI    string
	Title      string
	Servers    []string
	Operations []Operation
	// Skipped lists operations that could not be turned into a callable shape.
	Skipped []Skip

	root  map[string]any
	nodes int // nodes expanded for the operation being read
}

// Skip records an operation that was left out and why.
type Skip struct {
	Method, Path, OperationID, Reason string
}

// Operation is one method on one path.
type Operation struct {
	ID          string
	Method      string // upper case
	Path        string
	Summary     string
	Description string
	Tags        []string
	Deprecated  bool
	Parameters  []Parameter
	Body        *Body
}

// Parameter is a path, query or header parameter. Cookie parameters are
// dropped when the document is parsed.
type Parameter struct {
	Name        string
	In          string // path | query | header
	Description string
	Required    bool
	Style       string
	Explode     *bool
	Schema      map[string]any
}

// Body is a JSON request body.
type Body struct {
	Required    bool
	Description string
	ContentType string
	Schema      map[string]any
}

var methods = []string{"get", "put", "post", "delete", "patch", "head", "options"}

// Parse reads a JSON or YAML OpenAPI 3.x document.
func Parse(data []byte) (*Document, error) {
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse OpenAPI document: %w", err)
	}
	version, _ := root["openapi"].(string)
	if !strings.HasPrefix(version, "3.") {
		if _, ok := root["swagger"]; ok {
			return nil, fmt.Errorf("swagger 2.0 documents are not supported; convert to OpenAPI 3")
		}
		return nil, fmt.Errorf("not an OpenAPI 3 document (openapi=%q)", version)
	}
	d := &Document{OpenAPI: version, root: root}
	if info, ok := root["info"].(map[string]any); ok {
		d.Title, _ = info["title"].(string)
	}
	for _, s := range asSlice(root["servers"]) {
		if m, ok := s.(map[string]any); ok {
			if u, ok := m["url"].(string); ok && u != "" {
				d.Servers = append(d.Servers, u)
			}
		}
	}
	paths, _ := root["paths"].(map[string]any)
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, path := range keys {
		// The path is appended to the base URL: anything but an absolute
		// path could change the host the credential is sent to.
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#\\") || strings.HasPrefix(path, "//") {
			d.Skipped = append(d.Skipped, Skip{Path: path, Reason: "path is not an absolute path"})
			continue
		}
		item, ok := paths[path].(map[string]any)
		if !ok {
			continue
		}
		if _, isRef := item["$ref"]; isRef {
			d.nodes = 0
			if item, ok = d.resolve(item, 0, nil).(map[string]any); !ok {
				continue
			}
		}
		shared := asSlice(item["parameters"])
		for _, method := range methods {
			raw, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			d.nodes = 0
			op, reason := d.operation(strings.ToUpper(method), path, raw, shared)
			if reason == "" && d.nodes > MaxNodes {
				reason = "schemas are too large after $ref expansion"
			}
			if reason != "" {
				d.Skipped = append(d.Skipped, Skip{Method: strings.ToUpper(method), Path: path, OperationID: op.ID, Reason: reason})
				continue
			}
			d.Operations = append(d.Operations, op)
		}
	}
	return d, nil
}

func (d *Document) operation(method, path string, raw map[string]any, shared []any) (Operation, string) {
	op := Operation{Method: method, Path: path}
	op.ID, _ = raw["operationId"].(string)
	op.Summary, _ = raw["summary"].(string)
	op.Description, _ = raw["description"].(string)
	op.Deprecated, _ = raw["deprecated"].(bool)
	for _, t := range asSlice(raw["tags"]) {
		if s, ok := t.(string); ok {
			op.Tags = append(op.Tags, s)
		}
	}
	// Operation-level parameters override path-level ones with the same name
	// and location.
	byKey := map[string]int{}
	add := func(list []any) string {
		for _, p := range list {
			m, ok := d.resolve(p, 0, nil).(map[string]any)
			if !ok {
				continue
			}
			param := Parameter{}
			param.Name, _ = m["name"].(string)
			param.In, _ = m["in"].(string)
			param.Description, _ = m["description"].(string)
			param.Required, _ = m["required"].(bool)
			param.Style, _ = m["style"].(string)
			if e, ok := m["explode"].(bool); ok {
				param.Explode = &e
			}
			if param.Name == "" {
				continue
			}
			switch param.In {
			case "path":
				param.Required = true
				if param.Style != "" && param.Style != "simple" {
					return fmt.Sprintf("path parameter %q uses unsupported style %q", param.Name, param.Style)
				}
			case "query":
				if param.Style != "" && param.Style != "form" {
					return fmt.Sprintf("query parameter %q uses unsupported style %q", param.Name, param.Style)
				}
			case "header":
			case "cookie":
				continue
			default:
				return fmt.Sprintf("parameter %q has unsupported location %q", param.Name, param.In)
			}
			if s, ok := m["schema"].(map[string]any); ok {
				param.Schema = Clean(s)
			} else if _, ok := m["content"]; ok {
				return fmt.Sprintf("parameter %q uses content encoding", param.Name)
			} else {
				param.Schema = map[string]any{"type": "string"}
			}
			key := param.In + "\x00" + strings.ToLower(param.Name)
			if i, ok := byKey[key]; ok {
				op.Parameters[i] = param
				continue
			}
			byKey[key] = len(op.Parameters)
			op.Parameters = append(op.Parameters, param)
		}
		return ""
	}
	if reason := add(shared); reason != "" {
		return op, reason
	}
	if reason := add(asSlice(raw["parameters"])); reason != "" {
		return op, reason
	}
	if rb, ok := d.resolve(raw["requestBody"], 0, nil).(map[string]any); ok {
		content, _ := rb["content"].(map[string]any)
		ct := pickJSON(content)
		if ct == "" {
			if required, _ := rb["required"].(bool); required || len(content) > 0 {
				return op, "request body has no JSON media type"
			}
		} else {
			body := &Body{ContentType: ct}
			body.Required, _ = rb["required"].(bool)
			body.Description, _ = rb["description"].(string)
			media, _ := content[ct].(map[string]any)
			if s, ok := d.resolve(media["schema"], 0, nil).(map[string]any); ok {
				body.Schema = Clean(s)
			} else {
				body.Schema = map[string]any{}
			}
			op.Body = body
		}
	}
	return op, ""
}

func pickJSON(content map[string]any) string {
	keys := make([]string, 0, len(content))
	for k := range content {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		mt := strings.ToLower(strings.TrimSpace(strings.SplitN(k, ";", 2)[0]))
		if mt == "application/json" {
			return k
		}
	}
	for _, k := range keys {
		mt := strings.ToLower(strings.TrimSpace(strings.SplitN(k, ";", 2)[0]))
		if strings.HasSuffix(mt, "+json") {
			return k
		}
	}
	return ""
}

// resolve expands local $refs in v. stack holds the refs being expanded so a
// recursive schema is cut off instead of looping.
func (d *Document) resolve(v any, depth int, stack []string) any {
	d.nodes++
	if d.nodes > MaxNodes {
		return map[string]any{}
	}
	switch t := v.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok {
			if depth >= MaxRefDepth {
				return map[string]any{}
			}
			for _, s := range stack {
				if s == ref {
					return map[string]any{"description": "Recursive " + refName(ref) + " (any value)."}
				}
			}
			target, ok := d.pointer(ref)
			if !ok {
				return map[string]any{}
			}
			resolved := d.resolve(target, depth+1, append(stack, ref))
			// OpenAPI 3.1 allows siblings next to $ref; they refine the target.
			if len(t) > 1 {
				if m, ok := resolved.(map[string]any); ok {
					out := make(map[string]any, len(m)+len(t))
					for k, v := range m {
						out[k] = v
					}
					for k, v := range t {
						if k != "$ref" {
							out[k] = d.resolve(v, depth, stack)
						}
					}
					return out
				}
			}
			return resolved
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = d.resolve(val, depth, stack)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = d.resolve(val, depth, stack)
		}
		return out
	default:
		return v
	}
}

func (d *Document) pointer(ref string) (any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false // remote refs are not fetched
	}
	var cur any = d.root
	for _, part := range strings.Split(ref[2:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func refName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// dropped are OpenAPI-only keys that a tool schema does not need. Vendor
// extensions (x-*) are dropped too.
var dropped = map[string]bool{
	"xml": true, "externalDocs": true, "discriminator": true,
	"example": true, "examples": true, "deprecated": true,
	"$schema": true, "$id": true, "$defs": true, "definitions": true,
	"readOnly": true, "writeOnly": true,
}

// Clean returns a copy of an expanded schema with documentation-only keys
// removed and OpenAPI 3.0 `nullable` folded into the type, so the result is
// a plain JSON Schema a model API accepts.
func Clean(s map[string]any) map[string]any {
	out := make(map[string]any, len(s))
	for k, v := range s {
		if dropped[k] || strings.HasPrefix(k, "x-") {
			continue
		}
		switch k {
		case "properties", "patternProperties":
			if props, ok := v.(map[string]any); ok {
				clean := make(map[string]any, len(props))
				for name, p := range props {
					if pm, ok := p.(map[string]any); ok {
						if ro, _ := pm["readOnly"].(bool); ro {
							continue // the server sets these; a caller cannot
						}
						clean[name] = Clean(pm)
					}
				}
				out[k] = clean
				continue
			}
		case "nullable":
			continue
		}
		out[k] = cleanValue(v)
	}
	if nullable, _ := s["nullable"].(bool); nullable {
		if t, ok := out["type"].(string); ok {
			out["type"] = []any{t, "null"}
		}
	}
	if req, ok := out["required"].([]any); ok {
		if props, ok := out["properties"].(map[string]any); ok {
			kept := req[:0:0]
			for _, r := range req {
				if name, ok := r.(string); ok {
					if _, present := props[name]; present {
						kept = append(kept, name)
					}
				}
			}
			out["required"] = kept
		}
	}
	return out
}

func cleanValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return Clean(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = cleanValue(e)
		}
		return out
	default:
		return v
	}
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

var nonWord = regexp.MustCompile(`\W`)

// ShortID strips the path-and-method suffix that FastAPI appends to
// generated operation IDs ("search_stays_v1_search_stays_post" becomes
// "search_stays"). Other IDs are returned unchanged.
func (op Operation) ShortID() string {
	if op.ID == "" {
		return ""
	}
	suffix := nonWord.ReplaceAllString(op.Path, "_") + "_" + strings.ToLower(op.Method)
	if strings.HasSuffix(op.ID, suffix) && len(op.ID) > len(suffix) {
		return op.ID[:len(op.ID)-len(suffix)]
	}
	return op.ID
}
