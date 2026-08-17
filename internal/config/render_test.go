package config

import (
	"strings"
	"testing"
)

func svcEndpoint(name, group, host string, port int32) Endpoint {
	return Endpoint{
		Source:    SourceService,
		SourceRef: strings.ToLower(group) + "/" + strings.ToLower(name),
		Name:      name,
		Group:     group,
		Host:      host,
		Port:      port,
	}
}

func TestRenderAppliesDefaultTemplateByScheme(t *testing.T) {
	templates := NewTemplateSet(realWorldTemplates(t))
	r := NewRenderer(RenderOptions{})

	got := r.Render([]Endpoint{
		svcEndpoint("Web", "Storefront", "web.storefront.svc.cluster.local", 8096),
	}, templates)

	if len(got.Endpoints) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(got.Endpoints))
	}
	ep := got.Endpoints[0]

	if want := "http://web.storefront.svc.cluster.local:8096"; ep["url"] != want {
		t.Errorf("url = %v, want %v", ep["url"], want)
	}
	// The default-http template applied without the workload naming it.
	if ep["interval"] != "1m" {
		t.Errorf("interval = %v, want 1m from the default template", ep["interval"])
	}
	if ep["conditions"] == nil || ep["alerts"] == nil {
		t.Errorf("endpoint = %#v, want conditions and alerts from the template chain", ep)
	}
	if got.TemplateUsage["default-http"] != 1 {
		t.Errorf("TemplateUsage = %v, want default-http used once", got.TemplateUsage)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", got.Warnings)
	}
}

func TestRenderSchemeSelectsTemplate(t *testing.T) {
	templates := NewTemplateSet(realWorldTemplates(t))
	r := NewRenderer(RenderOptions{})

	ep := svcEndpoint("Cluster", "Shop", "database-rw.shop.svc.cluster.local", 5432)
	ep.Scheme = "tcp"

	got := r.Render([]Endpoint{ep}, templates)
	out := got.Endpoints[0]

	if want := "tcp://database-rw.shop.svc.cluster.local:5432"; out["url"] != want {
		t.Errorf("url = %v, want %v", out["url"], want)
	}
	conditions, _ := out["conditions"].([]any)
	if len(conditions) != 1 || conditions[0] != "[CONNECTED] == true" {
		t.Errorf("conditions = %#v, want the tcp template's", out["conditions"])
	}
}

func TestRenderTemplateImpliesScheme(t *testing.T) {
	// The workload names default-tcp but no scheme. The template's scheme has to
	// reach the URL, which is why URL assembly is deferred to render time.
	templates := NewTemplateSet(realWorldTemplates(t))
	r := NewRenderer(RenderOptions{})

	ep := svcEndpoint("Cache", "Docs", "cache.docs.svc.cluster.local", 6379)
	ep.Templates, ep.TemplatesSet = []string{"default-tcp"}, true

	out := r.Render([]Endpoint{ep}, templates).Endpoints[0]
	if want := "tcp://cache.docs.svc.cluster.local:6379"; out["url"] != want {
		t.Errorf("url = %v, want %v", out["url"], want)
	}
}

func TestRenderExplicitTemplateReplacesDefaults(t *testing.T) {
	templates := NewTemplateSet(append(realWorldTemplates(t),
		tpl(t, "bare", "http", nil, nil, "interval: 30s")))
	r := NewRenderer(RenderOptions{})

	ep := svcEndpoint("X", "G", "x.g.svc.cluster.local", 80)
	ep.Templates, ep.TemplatesSet = []string{"bare"}, true

	out := r.Render([]Endpoint{ep}, templates).Endpoints[0]
	if out["interval"] != "30s" {
		t.Errorf("interval = %v, want 30s", out["interval"])
	}
	// default-http was not applied, so its conditions must be absent.
	if out["conditions"] != nil {
		t.Errorf("conditions = %#v, want none: the explicit template replaces the defaults", out["conditions"])
	}
}

func TestRenderEmptyTemplateListMeansNoTemplates(t *testing.T) {
	templates := NewTemplateSet(realWorldTemplates(t))
	r := NewRenderer(RenderOptions{})

	ep := svcEndpoint("X", "G", "x.g.svc.cluster.local", 80)
	ep.TemplatesSet = true // an explicitly empty list

	out := r.Render([]Endpoint{ep}, templates).Endpoints[0]
	if out["conditions"] != nil || out["alerts"] != nil {
		t.Errorf("endpoint = %#v, want no template content at all", out)
	}
	if out["name"] != "X" {
		t.Errorf("name = %v, want X", out["name"])
	}
}

func TestRenderExtraTemplatesAppendAfterDefaults(t *testing.T) {
	templates := NewTemplateSet(append(realWorldTemplates(t),
		tpl(t, "urgent", "", nil, nil, "interval: 10s")))
	r := NewRenderer(RenderOptions{})

	ep := svcEndpoint("X", "G", "x.g.svc.cluster.local", 80)
	ep.ExtraTemplates = []string{"urgent"}

	out := r.Render([]Endpoint{ep}, templates).Endpoints[0]
	if out["interval"] != "10s" {
		t.Errorf("interval = %v, want 10s: extras merge after the defaults", out["interval"])
	}
	// The defaults still applied underneath.
	if out["conditions"] == nil {
		t.Errorf("conditions missing; defaultFor selection should still have run")
	}
}

func TestRenderPatchBeatsTemplates(t *testing.T) {
	// A workload overriding the template's conditions, end to end.
	templates := NewTemplateSet(realWorldTemplates(t))
	r := NewRenderer(RenderOptions{})

	ep := svcEndpoint("Portal", "Platform", "portal.platform.svc.cluster.local", 80)
	ep.Patch = mustYAML(t, `conditions: ["[STATUS] > 400", "[STATUS] < 500"]`)

	out := r.Render([]Endpoint{ep}, templates).Endpoints[0]
	conditions, _ := out["conditions"].([]any)
	if len(conditions) != 2 {
		t.Fatalf("conditions = %#v, want the patch's two entries, not the template's one", out["conditions"])
	}
	// Everything else from the template survives.
	if out["interval"] != "1m" || out["alerts"] == nil {
		t.Errorf("endpoint = %#v, want interval and alerts from the template", out)
	}
}

func TestRenderIdentityBeatsTemplates(t *testing.T) {
	// A template must never be able to rename what is being checked.
	templates := NewTemplateSet(append(realWorldTemplates(t),
		tpl(t, "meddling", "http", []string{"http"}, nil, "name: Wrong\nurl: http://wrong\ngroup: Wrong")))
	r := NewRenderer(RenderOptions{})

	out := r.Render([]Endpoint{
		svcEndpoint("Right", "Correct", "right.correct.svc.cluster.local", 80),
	}, templates).Endpoints[0]

	if out["name"] != "Right" || out["group"] != "Correct" {
		t.Errorf("identity = %v/%v, want Right/Correct", out["group"], out["name"])
	}
	if !strings.Contains(out["url"].(string), "right.correct") {
		t.Errorf("url = %v, want the derived one", out["url"])
	}
}

func TestRenderOmitsEmptyGroup(t *testing.T) {
	// Several endpoints in the source config sit at the top level.
	templates := NewTemplateSet(realWorldTemplates(t))
	out := NewRenderer(RenderOptions{}).Render([]Endpoint{
		svcEndpoint("Control plane", "", "control-plane.platform.svc.cluster.local", 80),
	}, templates).Endpoints[0]

	if _, present := out["group"]; present {
		t.Errorf("group = %#v, want the key omitted entirely", out["group"])
	}
}

func TestRenderExplicitURLIsUsedVerbatim(t *testing.T) {
	templates := NewTemplateSet(realWorldTemplates(t))
	ep := Endpoint{Source: SourceService, SourceRef: "ns/x", Name: "External", URL: "https://example.org", Scheme: "https"}
	ep.Patch = mustYAML(t, "interval: 5m")

	out := NewRenderer(RenderOptions{}).Render([]Endpoint{ep}, templates).Endpoints[0]
	if out["url"] != "https://example.org" {
		t.Errorf("url = %v, want https://example.org", out["url"])
	}
	if out["interval"] != "5m" {
		t.Errorf("interval = %v, want the patch's 5m", out["interval"])
	}
	// https is in default-http's defaultFor, so the template still applied.
	if out["conditions"] == nil {
		t.Errorf("conditions missing; https should select default-http")
	}
}

func TestRenderSortsByGroupThenName(t *testing.T) {
	templates := NewTemplateSet(realWorldTemplates(t))
	// Deliberately supplied out of order: the output must not depend on the
	// order objects happened to be discovered in.
	got := NewRenderer(RenderOptions{}).Render([]Endpoint{
		svcEndpoint("Zeta", "Storefront", "zeta.storefront.svc.cluster.local", 8080),
		svcEndpoint("Beta", "Platform", "beta.platform.svc.cluster.local", 8080),
		svcEndpoint("Alpha", "Storefront", "alpha.storefront.svc.cluster.local", 8080),
	}, templates)

	var order []string
	for _, ep := range got.Endpoints {
		order = append(order, ep["group"].(string)+"/"+ep["name"].(string))
	}

	want := []string{"Platform/Beta", "Storefront/Alpha", "Storefront/Zeta"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	templates := NewTemplateSet(realWorldTemplates(t))
	r := NewRenderer(RenderOptions{})
	endpoints := []Endpoint{
		svcEndpoint("B", "G", "b.g.svc.cluster.local", 2),
		svcEndpoint("A", "G", "a.g.svc.cluster.local", 1),
	}

	first, err := Marshal(Assemble(Object{}, r.Render(endpoints, templates).Endpoints))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := Marshal(Assemble(Object{}, r.Render(endpoints, NewTemplateSet(realWorldTemplates(t))).Endpoints))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(again) != string(first) {
			t.Fatalf("render %d differs:\n%s\nvs\n%s", i, again, first)
		}
	}
}

func TestRenderDedupsByURL(t *testing.T) {
	templates := NewTemplateSet(realWorldTemplates(t))

	fromService := svcEndpoint("Shop", "Shop", "shop.shop.svc.cluster.local", 2283)
	fromRoute := fromService
	fromRoute.Source = SourceIngressRoute
	fromRoute.SourceRef = "shop/shop-route"
	fromRoute.Name = "Shop route"

	got := NewRenderer(RenderOptions{}).Render([]Endpoint{fromRoute, fromService}, templates)

	if len(got.Endpoints) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(got.Endpoints))
	}
	// The Service wins regardless of input order: annotating it is the more
	// deliberate statement of intent.
	if got.Endpoints[0]["name"] != "Shop" {
		t.Errorf("kept %v, want the Service-derived Shop", got.Endpoints[0]["name"])
	}
	// Silently: an annotated Service outranking the route pointing at it is the
	// documented precedence, and warning would leave one standing forever on a
	// configuration that is behaving as designed.
	if len(got.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none for documented precedence", got.Warnings)
	}
}

func TestRenderDropsDuplicateGroupAndName(t *testing.T) {
	// Gatus keys stored history on group and name, so a collision would
	// interleave two services' results.
	templates := NewTemplateSet(realWorldTemplates(t))
	got := NewRenderer(RenderOptions{}).Render([]Endpoint{
		svcEndpoint("Cluster", "Tasks", "database-rw.tasks.svc.cluster.local", 5432),
		svcEndpoint("Cluster", "Tasks", "cluster-ro.tasks.svc.cluster.local", 5432),
	}, templates)

	if len(got.Endpoints) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(got.Endpoints))
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "already used by") {
		t.Errorf("Warnings = %v, want one naming the clash", got.Warnings)
	}
}

func TestRenderSameNameInDifferentGroupsIsFine(t *testing.T) {
	// "Cluster" and "Cache" repeat across groups throughout the source config.
	templates := NewTemplateSet(realWorldTemplates(t))
	got := NewRenderer(RenderOptions{}).Render([]Endpoint{
		svcEndpoint("Cluster", "Tasks", "database-rw.tasks.svc.cluster.local", 5432),
		svcEndpoint("Cluster", "Shop", "database-rw.shop.svc.cluster.local", 5432),
	}, templates)

	if len(got.Endpoints) != 2 {
		t.Fatalf("got %d endpoints, want 2", len(got.Endpoints))
	}
	if len(got.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", got.Warnings)
	}
}

func TestRenderBrokenTemplateDoesNotDropTheEndpoint(t *testing.T) {
	templates := NewTemplateSet(realWorldTemplates(t))
	ep := svcEndpoint("X", "G", "x.g.svc.cluster.local", 80)
	ep.Templates, ep.TemplatesSet = []string{"default-http", "does-not-exist"}, true

	got := NewRenderer(RenderOptions{}).Render([]Endpoint{ep}, templates)
	if len(got.Endpoints) != 1 {
		t.Fatalf("got %d endpoints, want the endpoint kept", len(got.Endpoints))
	}
	// The template that did resolve still contributed.
	if got.Endpoints[0]["interval"] != "1m" {
		t.Errorf("interval = %v, want 1m", got.Endpoints[0]["interval"])
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "does-not-exist") {
		t.Errorf("Warnings = %v, want one naming the missing template", got.Warnings)
	}
}

func TestRenderNoEndpointsProducesEmptyList(t *testing.T) {
	got := NewRenderer(RenderOptions{}).Render(nil, NewTemplateSet(nil))
	if got.Endpoints == nil {
		t.Fatal("Endpoints = nil, want an empty non-nil list")
	}
	if len(got.Endpoints) != 0 {
		t.Errorf("Endpoints = %v, want empty", got.Endpoints)
	}
}

func TestAssemblePreservesBaseAndReplacesEndpoints(t *testing.T) {
	base := mustYAML(t, `
storage:
  type: postgres
  path: "postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@host:5432/db?sslmode=disable"
alerting:
  telegram:
    token: "${TELEGRAM_TOKEN}"
ui:
  default-sort-by: group
announcements: []
`)
	endpoints := []Object{{"name": "X", "url": "http://x"}}

	got := Assemble(base, endpoints)

	list, ok := got["endpoints"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("endpoints = %#v, want a one-item list", got["endpoints"])
	}
	if got["ui"] == nil || got["announcements"] == nil {
		t.Errorf("assembled = %#v, want the base keys preserved", got)
	}
	// Assemble must not mutate the caller's base, which is reused every render.
	if _, leaked := base["endpoints"]; leaked {
		t.Error("Assemble mutated the base config")
	}

	out, err := Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Placeholders are Gatus's to expand, not the sidecar's.
	for _, want := range []string{"${POSTGRES_PASSWORD}", "${TELEGRAM_TOKEN}"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lost %s:\n%s", want, out)
		}
	}
}

func TestAssembleDropsEndpointsFromBase(t *testing.T) {
	// LoadBase strips it, but Assemble must not reintroduce a stale list either.
	base := Object{"ui": Object{"default-sort-by": "group"}}
	got := Assemble(base, []Object{{"name": "New"}})

	list := got["endpoints"].([]any)
	if len(list) != 1 || list[0].(Object)["name"] != "New" {
		t.Errorf("endpoints = %#v, want only the rendered list", got["endpoints"])
	}
}

// An annotated Service beating the IngressRoute that points at the same
// workload is the documented precedence, not a problem to report. Alerting on
// render warnings would otherwise fire forever on a healthy configuration.
func TestRenderPrecedenceOverTheSameWorkloadIsNotAWarning(t *testing.T) {
	svc := Endpoint{
		Source: SourceService, SourceRef: "shop/api",
		Name: "Api", Group: "Shop",
		Host: "api.shop.svc.cluster.local", Port: 3000, Path: "/health",
	}
	fromRoute := Endpoint{
		Source: SourceIngressRoute, SourceRef: "shop/api-ingress",
		Name: "Api", Group: "Shop",
		Host: "api.shop.svc.cluster.local", Port: 3000, Path: "/api",
	}

	res := NewRenderer(RenderOptions{}).Render([]Endpoint{svc, fromRoute}, NewTemplateSet(nil))

	if len(res.Endpoints) != 1 {
		t.Fatalf("rendered %d endpoints, want 1", len(res.Endpoints))
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v, want none for documented precedence", res.Warnings)
	}
}

// Two unrelated workloads claiming one name does lose a check, and is worth
// saying out loud.
func TestRenderNameClashBetweenDifferentWorkloadsWarns(t *testing.T) {
	a := Endpoint{
		Source: SourceService, SourceRef: "shop/api",
		Name: "Api", Group: "Shop", Host: "api.shop.svc.cluster.local", Port: 3000,
	}
	b := Endpoint{
		Source: SourceService, SourceRef: "shop/api-v2",
		Name: "Api", Group: "Shop", Host: "api-v2.shop.svc.cluster.local", Port: 3000,
	}

	res := NewRenderer(RenderOptions{}).Render([]Endpoint{a, b}, NewTemplateSet(nil))

	if len(res.Endpoints) != 1 {
		t.Fatalf("rendered %d endpoints, want 1", len(res.Endpoints))
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", res.Warnings)
	}
}

// One object yielding the same address twice is the sidecar collapsing it,
// usually because a path annotation overrode what told two routes apart.
func TestRenderDuplicateURLFromOneObjectIsNotAWarning(t *testing.T) {
	dup := func(name string) Endpoint {
		return Endpoint{
			Source: SourceIngressRoute, SourceRef: "shop/shop-ingress",
			Name: name, Group: "Shop", URL: "https://shop.example.org",
		}
	}

	res := NewRenderer(RenderOptions{}).Render([]Endpoint{dup("A"), dup("B")}, NewTemplateSet(nil))

	if len(res.Endpoints) != 1 {
		t.Fatalf("rendered %d endpoints, want 1", len(res.Endpoints))
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", res.Warnings)
	}
}

// An annotated Service naming a health path used to defeat dedup: the URLs no
// longer matched, so the route's inferred in-cluster check survived alongside
// it and failed on its own.
func TestRenderDropsInferredCheckOfADirectlyMonitoredService(t *testing.T) {
	svc := Endpoint{
		Source: SourceService, SourceRef: "nextcloud/nextcloud",
		Name: "Nextcloud", Group: "Nextcloud",
		Host: "nextcloud.nextcloud.svc.cluster.local", Port: 80, Path: "/status.php",
	}
	inferred := Endpoint{
		Source: SourceIngressRoute, SourceRef: "nextcloud/nextcloud-ingress",
		Name: "Nextcloud ingress", Group: "Nextcloud",
		Host: "nextcloud.nextcloud.svc.cluster.local", Port: 80,
	}
	external := Endpoint{
		Source: SourceIngressRoute, SourceRef: "nextcloud/nextcloud-ingress",
		Name: "Nextcloud ingress (external)", Group: "Nextcloud",
		URL: "https://cloud.example.org", Scheme: "https",
	}

	res := NewRenderer(RenderOptions{}).Render([]Endpoint{svc, inferred, external}, NewTemplateSet(nil))

	if len(res.Endpoints) != 2 {
		t.Fatalf("rendered %d endpoints, want 2: %v", len(res.Endpoints), res.Endpoints)
	}
	names := map[string]bool{}
	for _, ep := range res.Endpoints {
		names[stringValue(ep, "name")] = true
	}
	if !names["Nextcloud"] || !names["Nextcloud ingress (external)"] {
		t.Errorf("kept %v, want the Service check and the public one", names)
	}
	// The public endpoint is the only thing covering DNS, TLS and the proxy.
	if names["Nextcloud ingress"] {
		t.Error("the inferred in-cluster check survived")
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v, want none for documented precedence", res.Warnings)
	}
}

// Two routes to one Service at different paths are both inferred, so neither
// outranks the other and both stay.
func TestRenderKeepsSeveralInferredChecksWhenNoServiceIsAnnotated(t *testing.T) {
	a := Endpoint{
		Source: SourceIngressRoute, SourceRef: "shop/ingress",
		Name: "A", Group: "Shop", Host: "web.shop.svc.cluster.local", Port: 80,
	}
	b := Endpoint{
		Source: SourceIngressRoute, SourceRef: "shop/ingress",
		Name: "B", Group: "Shop", Host: "web.shop.svc.cluster.local", Port: 80, Path: "/api",
	}

	res := NewRenderer(RenderOptions{}).Render([]Endpoint{a, b}, NewTemplateSet(nil))

	if len(res.Endpoints) != 2 {
		t.Fatalf("rendered %d endpoints, want 2: %v", len(res.Endpoints), res.Endpoints)
	}
}

// A Service annotated on a different port is a different thing to check.
func TestRenderKeepsInferredCheckOnAnotherPort(t *testing.T) {
	svc := Endpoint{
		Source: SourceService, SourceRef: "shop/web",
		Name: "Web", Group: "Shop", Host: "web.shop.svc.cluster.local", Port: 8080,
	}
	inferred := Endpoint{
		Source: SourceIngressRoute, SourceRef: "shop/ingress",
		Name: "Web ingress", Group: "Shop", Host: "web.shop.svc.cluster.local", Port: 80,
	}

	res := NewRenderer(RenderOptions{}).Render([]Endpoint{svc, inferred}, NewTemplateSet(nil))

	if len(res.Endpoints) != 2 {
		t.Fatalf("rendered %d endpoints, want 2: %v", len(res.Endpoints), res.Endpoints)
	}
}
