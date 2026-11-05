package connectors

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Manifest is everything about an app that its OpenAPI document does not
// say, or says wrongly: how credentials are sent, how to check them, where
// the API lives and which operations become tools.
type Manifest struct {
	// Slug names the app in tool names (BLUEPILLOW_SEARCH_STAYS). Lower case
	// letters, digits and underscores.
	Slug        string `yaml:"slug" json:"slug"`
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Spec is the OpenAPI document: an https URL, or a path relative to
	// the manifest file.
	Spec string `yaml:"spec" json:"spec"`
	// BaseURL overrides the document's first server. Required when the
	// document declares none.
	BaseURL string `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	Docs    string `yaml:"docs,omitempty" json:"docs,omitempty"`
	Auth    Auth   `yaml:"auth" json:"auth"`
	// Check is a cheap authenticated request used to validate credentials
	// before they are saved. Without one, credentials cannot be checked.
	Check *Check     `yaml:"check,omitempty" json:"check,omitempty"`
	Tools ToolFilter `yaml:"tools,omitempty" json:"tools,omitempty"`
}

// AuthType is how a credential reaches the API.
type AuthType string

const (
	// AuthBearer sends "Authorization: Bearer <api_key>".
	AuthBearer AuthType = "bearer"
	// AuthHeader sends "<name>: <prefix><api_key>".
	AuthHeader AuthType = "header"
	// AuthQuery sends "?<name>=<api_key>".
	AuthQuery AuthType = "query"
	// AuthBasic sends HTTP Basic with username and password.
	AuthBasic AuthType = "basic"
	// AuthNone sends nothing.
	AuthNone AuthType = "none"
)

// Auth describes the app's credential and how it is sent.
type Auth struct {
	Type AuthType `yaml:"type" json:"type"`
	// Name is the header or query parameter for header and query auth.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	// Prefix is prepended to the key for header auth, e.g. "Token ".
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	// Fields are what a person is asked for. Defaults: api_key, or
	// username and password for basic auth.
	Fields []Field `yaml:"fields,omitempty" json:"fields,omitempty"`
}

// Field is one credential input, enough to render a form for it.
type Field struct {
	Name  string `yaml:"name" json:"name"`
	Label string `yaml:"label,omitempty" json:"label,omitempty"`
	Help  string `yaml:"help,omitempty" json:"help,omitempty"`
	// URL is where a person gets the value.
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
	// Public marks a value that is not a secret (an anonymous rate-limit
	// key, a username). Fields are secret unless marked public.
	Public bool `yaml:"public,omitempty" json:"public,omitempty"`
}

// Check is the credential-validation request. Path may reference credential
// fields as {field}; their values are path-escaped.
type Check struct {
	Method string `yaml:"method,omitempty" json:"method,omitempty"`
	Path   string `yaml:"path" json:"path"`
	// RejectedStatus lists the statuses that mean "these credentials are
	// wrong". Defaults to 401 and 403. An endpoint that looks the key up in
	// its own path may answer an unknown key with 404.
	RejectedStatus []int `yaml:"rejected_status,omitempty" json:"rejected_status,omitempty"`
}

// ToolFilter selects operations by operation ID (full or FastAPI-short) or
// by tool name.
type ToolFilter struct {
	Include []string          `yaml:"include,omitempty" json:"include,omitempty"`
	Exclude []string          `yaml:"exclude,omitempty" json:"exclude,omitempty"`
	Names   map[string]string `yaml:"names,omitempty" json:"names,omitempty"`
	// AllowWrites keeps non-GET operations. Defaults to true; set false for
	// a read-only connector.
	AllowWrites *bool `yaml:"allow_writes,omitempty" json:"allow_writes,omitempty"`
}

var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// ParseManifest reads a YAML (or JSON) manifest and validates it.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	return m, m.Validate()
}

// Validate checks the manifest and fills defaults in place.
func (m *Manifest) Validate() error {
	if !slugPattern.MatchString(m.Slug) {
		return fmt.Errorf("manifest slug %q must match %s", m.Slug, slugPattern)
	}
	if strings.TrimSpace(m.Spec) == "" {
		return fmt.Errorf("manifest %s: spec is required", m.Slug)
	}
	if m.BaseURL != "" {
		if err := checkBaseURL(m.BaseURL); err != nil {
			return fmt.Errorf("manifest %s: %w", m.Slug, err)
		}
	}
	a := &m.Auth
	switch a.Type {
	case AuthBearer, AuthBasic, AuthNone:
	case AuthHeader, AuthQuery:
		if strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("manifest %s: %s auth needs a name", m.Slug, a.Type)
		}
	case "":
		return fmt.Errorf("manifest %s: auth.type is required", m.Slug)
	default:
		return fmt.Errorf("manifest %s: unsupported auth type %q", m.Slug, a.Type)
	}
	if len(a.Fields) == 0 {
		switch a.Type {
		case AuthBasic:
			a.Fields = []Field{{Name: "username", Label: "Username", Public: true}, {Name: "password", Label: "Password"}}
		case AuthNone:
		default:
			a.Fields = []Field{{Name: "api_key", Label: "API key"}}
		}
	}
	seen := map[string]bool{}
	for _, f := range a.Fields {
		if !fieldPattern.MatchString(f.Name) || seen[f.Name] {
			return fmt.Errorf("manifest %s: bad or duplicate credential field %q", m.Slug, f.Name)
		}
		seen[f.Name] = true
	}
	if a.Type == AuthBasic && (!seen["username"] || !seen["password"]) {
		return fmt.Errorf("manifest %s: basic auth needs username and password fields", m.Slug)
	}
	if m.Check != nil {
		if m.Check.Method == "" {
			m.Check.Method = http.MethodGet
		}
		m.Check.Method = strings.ToUpper(m.Check.Method)
		if !strings.HasPrefix(m.Check.Path, "/") {
			return fmt.Errorf("manifest %s: check.path must start with /", m.Slug)
		}
		if len(m.Check.RejectedStatus) == 0 {
			m.Check.RejectedStatus = []int{http.StatusUnauthorized, http.StatusForbidden}
		}
		for _, s := range m.Check.RejectedStatus {
			if s < 400 || s > 499 {
				return fmt.Errorf("manifest %s: check.rejected_status %d is not a 4xx status", m.Slug, s)
			}
		}
		for _, name := range placeholders(m.Check.Path) {
			if !seen[name] {
				return fmt.Errorf("manifest %s: check.path references unknown field %q", m.Slug, name)
			}
		}
	}
	return nil
}

// keyField is the field whose value bearer, header and query auth send.
func (a Auth) keyField() string {
	if len(a.Fields) == 0 {
		return ""
	}
	return a.Fields[0].Name
}

var (
	fieldPattern       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	placeholderPattern = regexp.MustCompile(`\{([^{}]+)\}`)
)

func placeholders(path string) []string {
	var out []string
	for _, m := range placeholderPattern.FindAllStringSubmatch(path, -1) {
		out = append(out, m[1])
	}
	return out
}

func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid base URL %q", raw)
	}
	if u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return fmt.Errorf("base URL %q must use https", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("base URL %q must not carry credentials, a query or a fragment", raw)
	}
	return nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
