package discovery

import (
	"reflect"
	"testing"
)

func TestParseRule(t *testing.T) {
	tests := []struct {
		name      string
		rule      string
		wantHosts []string
		wantPath  string
	}{
		{
			name:      "bare host",
			rule:      "Host(`status.example.com`)",
			wantHosts: []string{"status.example.com"},
		},
		{
			name:      "host and path prefix",
			rule:      "Host(`shop.example.com`) && PathPrefix(`/api`)",
			wantHosts: []string{"shop.example.com"},
			wantPath:  "/api",
		},
		{
			name:      "several hosts in one matcher",
			rule:      "Host(`a.example.org`, `b.example.org`)",
			wantHosts: []string{"a.example.org", "b.example.org"},
		},
		{
			name:      "hosts joined with or",
			rule:      "Host(`a.example.org`) || Host(`b.example.org`)",
			wantHosts: []string{"a.example.org", "b.example.org"},
		},
		{
			name:      "duplicate hosts are collapsed",
			rule:      "(Host(`a.example.org`) && PathPrefix(`/x`)) || (Host(`a.example.org`) && PathPrefix(`/y`))",
			wantHosts: []string{"a.example.org"},
			wantPath:  "/x",
		},
		{
			// These do not change which address to check, but the rule still has
			// to parse or the whole route would be skipped.
			name:      "matchers the sidecar does not model are ignored",
			rule:      "Host(`a.example.org`) && Method(`GET`) && Headers(`X-Foo`, `bar`)",
			wantHosts: []string{"a.example.org"},
		},
		{
			name:      "client ip and query matchers",
			rule:      "Host(`a.example.org`) && ClientIP(`10.0.0.0/8`) && Query(`k`, `v`)",
			wantHosts: []string{"a.example.org"},
		},
		{
			name:      "whitespace inside the call",
			rule:      "Host( `a.example.org` )",
			wantHosts: []string{"a.example.org"},
		},
		{
			name:      "lowercase matcher names",
			rule:      "host(`a.example.org`) && pathprefix(`/api`)",
			wantHosts: []string{"a.example.org"},
			wantPath:  "/api",
		},
		{
			name:      "HostSNI is treated as a host",
			rule:      "HostSNI(`a.example.org`)",
			wantHosts: []string{"a.example.org"},
		},
		{
			// A regexp host is not one concrete address, so there is nothing to check.
			name:      "regexp hosts are skipped",
			rule:      "HostRegexp(`^.+\\.example\\.org$`)",
			wantHosts: nil,
		},
		{
			name:      "path is normalised",
			rule:      "Host(`a.example.org`) && PathPrefix(`api/health`)",
			wantHosts: []string{"a.example.org"},
			wantPath:  "/api/health",
		},
		{
			name:      "root path contributes nothing",
			rule:      "Host(`a.example.org`) && PathPrefix(`/`)",
			wantHosts: []string{"a.example.org"},
			wantPath:  "",
		},
		{
			name:     "no host at all",
			rule:     "PathPrefix(`/api`)",
			wantPath: "/api",
		},
		{
			name: "empty rule",
			rule: "",
		},
		{
			// The reason Traefik's parser is used instead of a pattern match:
			// this address is one the route deliberately does not serve.
			name:      "negated host is excluded",
			rule:      "!Host(`internal.example.org`)",
			wantHosts: nil,
		},
		{
			name:      "negation applies to the whole group",
			rule:      "Host(`a.example.org`) && !(Host(`b.example.org`) || Host(`c.example.org`))",
			wantHosts: []string{"a.example.org"},
		},
		{
			name:      "negated path is not used",
			rule:      "Host(`a.example.org`) && !PathPrefix(`/internal`)",
			wantHosts: []string{"a.example.org"},
			wantPath:  "",
		},
		{
			name:      "double negation is a positive match",
			rule:      "!(!Host(`a.example.org`))",
			wantHosts: []string{"a.example.org"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRule(tc.rule)
			if err != nil {
				t.Fatalf("parseRule(%q): %v", tc.rule, err)
			}
			if !reflect.DeepEqual(got.Hosts, tc.wantHosts) {
				t.Errorf("Hosts = %#v, want %#v", got.Hosts, tc.wantHosts)
			}
			if got.Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", got.Path, tc.wantPath)
			}
		})
	}
}

func TestParseRuleRejectsUnparseableRules(t *testing.T) {
	// A rule the parser cannot read yields an error rather than a guess, so the
	// operator finds out instead of silently losing an endpoint.
	tests := []string{
		"Host(`a.example.org`",
		"NotAMatcher(`x`)",
		"Host(`a.example.org`) &&",
	}

	for _, rule := range tests {
		t.Run(rule, func(t *testing.T) {
			if _, err := parseRule(rule); err == nil {
				t.Errorf("parseRule(%q) = nil error, want a failure", rule)
			}
		})
	}
}
