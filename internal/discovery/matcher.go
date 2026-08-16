package discovery

import (
	"fmt"
	"strings"

	traefikrules "github.com/traefik/traefik/v3/pkg/rules"
)

// parsedRule is what a Traefik match expression contributes to a URL.
type parsedRule struct {
	// Hosts are the concrete hostnames the rule matches, in the order written.
	Hosts []string
	// Path is the first path prefix in the rule, if any.
	Path string
}

// traefikMatchers is every matcher name Traefik's rule syntax accepts. The
// parser rejects any function it was not told about, so they all have to be
// listed even though only Host and PathPrefix affect which address to check.
// The rest are parsed and ignored.
var traefikMatchers = []string{
	// HTTP routers.
	"Host", "HostRegexp", "Path", "PathPrefix", "PathRegexp",
	"Header", "HeaderRegexp", "Headers", "HeadersRegexp",
	"Method", "Query", "QueryRegexp", "ClientIP",
	// TCP routers.
	"HostSNI", "HostSNIRegexp", "ALPN",
}

// hostMatchers name a concrete hostname. HostSNI is included because a TCP
// router's SNI value is the same address a client connects to.
var hostMatchers = map[string]bool{
	"host":    true,
	"hostsni": true,
}

// pathMatchers name a URL path prefix.
var pathMatchers = map[string]bool{
	"path":       true,
	"pathprefix": true,
}

// parseRule extracts hostnames and a path prefix from a Traefik match expression.
//
// Traefik's own parser is used rather than a pattern match, because the rule
// language has operator precedence, grouping and negation. In particular a
// negated matcher such as !Host(`internal.example.org`) describes an address the
// route deliberately does not serve, and monitoring it would report a failure
// for a URL that was never meant to work.
//
// A rule can legitimately name several hosts, either as multiple arguments to
// one matcher or as alternatives joined with ||. Each distinct host is a
// separate address worth checking, so they are all returned.
func parseRule(rule string) (parsedRule, error) {
	if strings.TrimSpace(rule) == "" {
		return parsedRule{}, nil
	}

	parser, err := traefikrules.NewParser(traefikMatchers)
	if err != nil {
		return parsedRule{}, fmt.Errorf("build rule parser: %w", err)
	}
	parsed, err := parser.Parse(rule)
	if err != nil {
		return parsedRule{}, fmt.Errorf("parse match rule %q: %w", rule, err)
	}
	builder, ok := parsed.(traefikrules.TreeBuilder)
	if !ok {
		return parsedRule{}, fmt.Errorf("parse match rule %q: unexpected result type %T", rule, parsed)
	}

	var out parsedRule
	walkTree(builder(), false, &out, make(map[string]bool))
	return out, nil
}

// walkTree collects hosts and a path from the rule tree.
//
// negated tracks whether the current subtree sits under a negation, so that
// !(Host(`a`) || Host(`b`)) excludes both rather than just the first.
func walkTree(t *traefikrules.Tree, negated bool, out *parsedRule, seen map[string]bool) {
	if t == nil {
		return
	}
	if t.Not {
		negated = !negated
	}

	matcher := strings.ToLower(t.Matcher)
	switch matcher {
	case "and", "or":
		walkTree(t.RuleLeft, negated, out, seen)
		walkTree(t.RuleRight, negated, out, seen)
		return
	}

	if negated {
		return
	}

	switch {
	case hostMatchers[matcher]:
		for _, host := range t.Value {
			// A host containing a regexp placeholder cannot be turned into one
			// concrete address, so it is skipped rather than checked literally.
			host = strings.TrimSpace(host)
			if host == "" || strings.ContainsAny(host, "{}") || seen[host] {
				continue
			}
			seen[host] = true
			out.Hosts = append(out.Hosts, host)
		}

	case pathMatchers[matcher]:
		if out.Path != "" {
			return
		}
		for _, path := range t.Value {
			if path = strings.TrimSpace(path); path == "" || strings.ContainsAny(path, "{}") {
				continue
			}
			out.Path = normalisePath(path)
			break
		}
	}
}
