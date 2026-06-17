package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// seedTemplateStore creates an in-memory store seeded with a predictable
// dataset: one global workflow (with two steps), two repo projects each with
// one workflow (with one step).
func seedTemplateStore(t *testing.T) *ProjectStore {
	t.Helper()
	ctx := context.Background()

	store, err := OpenMemoryProjectStore(ctx)
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	// Seed the two projects so they appear in the store's projects table.
	store.UpsertManagedProject(ctx, "nitecon/alpha", "refs/heads/main", "aaa111")
	store.UpsertManagedProject(ctx, "nitecon/beta", "refs/heads/main", "bbb222")

	// Global workflow with two steps.
	globalID, err := store.CreateWorkflow(ctx, &Workflow{
		Scope:     WorkflowScopeGlobal,
		EventType: "push",
		Name:      "global-ci",
		Enabled:   true,
		Nodes: []WorkflowNode{
			{NodeKey: "lint", Name: "Lint", Type: "command", Command: "make lint"},
			{NodeKey: "test", Name: "Test", Type: "command", Command: "make test"},
		},
	})
	if err != nil {
		t.Fatalf("create global workflow: %v", err)
	}
	_ = globalID

	// Repo workflow for nitecon/alpha.
	_, err = store.CreateWorkflow(ctx, &Workflow{
		Scope:     WorkflowScopeRepo,
		Repo:      "nitecon/alpha",
		EventType: "push",
		Name:      "alpha-deploy",
		Enabled:   true,
		Nodes: []WorkflowNode{
			{NodeKey: "deploy", Name: "Deploy", Type: "command", Command: "make deploy"},
		},
	})
	if err != nil {
		t.Fatalf("create alpha workflow: %v", err)
	}

	// Repo workflow for nitecon/beta.
	_, err = store.CreateWorkflow(ctx, &Workflow{
		Scope:     WorkflowScopeRepo,
		Repo:      "nitecon/beta",
		EventType: "pull_request",
		Name:      "beta-checks",
		Enabled:   true,
		Nodes: []WorkflowNode{
			{NodeKey: "check", Name: "Check", Type: "command", Command: "make check"},
		},
	})
	if err != nil {
		t.Fatalf("create beta workflow: %v", err)
	}

	return store
}

func TestDashboardTemplateControlPanelSkeleton(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	indexHandler(Config{}, store).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// Control-panel skeleton classes must be present.
	for _, required := range []string{
		`class="app-page"`,
		`class="app-layout`,
		`class="sidebar"`,
		`class="app-body"`,
		`class="app-content"`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("expected skeleton class %q in rendered output", required)
		}
	}
}

func TestDashboardTemplateNavSections(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	indexHandler(Config{}, store).ServeHTTP(rec, req)

	body := rec.Body.String()

	for _, section := range []string{"Organization", "Repositories"} {
		if !strings.Contains(body, section) {
			t.Errorf("expected nav section %q in rendered output", section)
		}
	}
	if strings.Contains(body, `<span class="nd-nav-section">Workflows</span>`) {
		t.Error("configuration must live in the header nav, not the repository sidebar")
	}

	if !strings.Contains(body, `class="nd-nav-section"`) {
		t.Error("expected nd-nav-section class in rendered output")
	}
	if strings.Contains(body, `<span class="nd-nav-section">nitecon/alpha</span>`) {
		t.Error("repository names must not be duplicated as sidebar section headings")
	}
	for _, href := range []string{`href="/global"`, `href="/nitecon/alpha"`, `href="/nitecon/beta"`} {
		if !strings.Contains(body, href) {
			t.Errorf("expected routed nav link %q in rendered output", href)
		}
	}
	if !strings.Contains(body, `href="/global">Configuration</a>`) {
		t.Error("expected dedicated Configuration link in the header nav")
	}
	if !strings.Contains(body, `data-eventic-version="`) {
		t.Error("expected running version badge in the header")
	}
}

func TestDashboardTemplateStepNames(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/nitecon/alpha", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	configurationHandler(store).ServeHTTP(rec, req)

	body := rec.Body.String()

	for _, stepName := range []string{"alpha-deploy", "Deploy", "make deploy", "Run Command"} {
		if !strings.Contains(body, stepName) {
			t.Errorf("expected workflow configuration text %q in rendered output", stepName)
		}
	}
}

func TestDashboardTemplateMainPanelIDs(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	indexHandler(Config{}, store).ServeHTTP(rec, req)

	body := rec.Body.String()

	// Main content panels must carry the expected ids.
	for _, id := range []string{`id="project-runs"`, `id="event-stream"`, `id="projects-overview"`} {
		if !strings.Contains(body, id) {
			t.Errorf("expected panel id %q in rendered output", id)
		}
	}
}

func TestDashboardTemplateDataNdAttributes(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	indexHandler(Config{}, store).ServeHTTP(rec, req)

	body := rec.Body.String()

	// Key data-nd-* attributes must be present.
	for _, attr := range []string{
		`data-nd-bind="/api/projects?org=${org}&amp;search=${repoSearch}&amp;summary=1"`,
		`data-nd-bind="/api/runs?limit=20"`,
		`data-nd-ws="${runs-ws}"`,
		`data-nd-ws-filter="type:run"`,
		`data-nd-mode="prepend"`,
		`data-nd-max="50"`,
	} {
		if !strings.Contains(body, attr) {
			t.Errorf("expected data-nd attribute %q in rendered output", attr)
		}
	}
}

func TestConfigurationTemplateGlobalWorkflows(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/global", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	configurationHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, expected := range []string{
		"Global Workflows",
		"global-ci",
		"Lint",
		"make lint",
		"Test",
		"make test",
		`data-eventic-action="POST /api/workflow-config?scope=global"`,
		`id="repo-nav-list"`,
		`href="/nitecon/alpha"`,
		`href="/nitecon/beta"`,
		`href="/global/events"`,
		`href="/global/mappings"`,
		`href="/global/mappings/advanced"`,
		`for="workflow-continue-on-error"`,
		`id="workflow-continue-on-error" type="checkbox"`,
		`name="action_type"`,
		`name="response_mode"`,
		`href="/global" class="nd-active">Configuration</a>`,
		`data-eventic-version="`,
		"eventicRefreshWorkflowForm",
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("expected global configuration page to contain %q", expected)
		}
	}
	for _, unwanted := range []string{
		"Step Name",
		"Post Action Name",
		"Action Type",
		`id="workflow-step-name"`,
		`class="nd-checkbox"`,
		`<span class="nd-nav-section">Navigation</span>`,
		`id="event-mapper"`,
		`id="stable-events-management"`,
		`id="event-mappings"`,
		`draggable="true"
                         data-eventic-provider-event`,
		`<div class="nd-well"
                     data-eventic-drop-zone`,
		`<select id="provider-event-filter" data-eventic-provider-filter>`,
		`data-eventic-action="POST /api/stable-events"`,
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("expected global configuration page not to contain redundant label %q", unwanted)
		}
	}
}

func TestConfigurationTemplateStableEventsPage(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/global/events", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	configurationHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, expected := range []string{
		"Stable Events",
		`id="stable-events-management"`,
		`data-eventic-action="POST /api/stable-events"`,
		`data-eventic-action="PUT /api/stable-events/`,
		`href="/global"`,
		`href="/global/mappings"`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("expected stable events page to contain %q", expected)
		}
	}
	for _, unwanted := range []string{
		`id="workflow-create"`,
		`id="workflow-configuration"`,
		`id="event-mapper"`,
		`id="event-mappings"`,
		`draggable="true"
                         data-eventic-provider-event`,
		`<div class="nd-well"
                     data-eventic-drop-zone`,
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("expected stable events page not to contain %q", unwanted)
		}
	}
}

func TestConfigurationTemplateVisualMapperPage(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/global/mappings", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	configurationHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, expected := range []string{
		"Event Mapper",
		`id="event-mapper"`,
		`id="provider-events"`,
		`id="stable-event-targets"`,
		`draggable="true"
                         data-eventic-provider-event`,
		`<div class="nd-well"
                     data-eventic-drop-zone`,
		`data-eventic-provider-filter`,
		`href="/global/events"`,
		`href="/global/mappings/advanced"`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("expected visual mapper page to contain %q", expected)
		}
	}
	for _, unwanted := range []string{
		`id="workflow-create"`,
		`id="workflow-configuration"`,
		`id="stable-events-management"`,
		`id="event-mappings"`,
		`data-eventic-action="POST /api/stable-events"`,
		`data-eventic-action="PUT /api/stable-events/`,
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("expected visual mapper page not to contain %q", unwanted)
		}
	}
}

func TestConfigurationTemplateAdvancedMappingsPage(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/global/mappings/advanced", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	configurationHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, expected := range []string{
		"Advanced Mappings",
		`id="event-mappings"`,
		`data-eventic-action="POST /api/event-mappings"`,
		`data-eventic-action="PUT /api/event-mappings/`,
		`for="mapping-conditions"`,
		`href="/global/mappings"`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("expected advanced mappings page to contain %q", expected)
		}
	}
	for _, unwanted := range []string{
		`id="workflow-create"`,
		`id="workflow-configuration"`,
		`id="stable-events-management"`,
		`id="event-mapper"`,
		`draggable="true"
                         data-eventic-provider-event`,
		`<div class="nd-well"
                     data-eventic-drop-zone`,
		`name="mapping_id"`,
		`for="mapping-id-`,
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("expected advanced mappings page not to contain %q", unwanted)
		}
	}
}

func TestConfigurationTemplateMissingRepo404s(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/nitecon/missing", nil)
	rec := httptest.NewRecorder()

	configurationHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown repo, got %d", rec.Code)
	}
}

func TestDashboardTemplateConformance(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	indexHandler(Config{}, store).ServeHTTP(rec, req)

	body := rec.Body.String()

	// Fix 1: runs binding must live on <tbody>, not on the section or table.
	// Confirm the tbody carries the bind and no stray bind remains on the section.
	if !strings.Contains(body, `<tbody data-nd-bind="/api/runs?limit=20"`) {
		t.Error("expected runs binding on <tbody>, not on section or table")
	}
	// The section itself must not carry a data-nd-bind (which would destroy table structure).
	if strings.Contains(body, `id="project-runs"`+"\n"+"                 data-nd-bind") ||
		strings.Contains(body, `id="project-runs" data-nd-bind`) {
		t.Error("section#project-runs must not carry data-nd-bind")
	}

	// Fix 2: no data-nd-action="GET #" on any anchor — set-only pattern.
	if strings.Contains(body, `data-nd-action="GET #"`) {
		t.Error("nav anchors must not use data-nd-action=\"GET #\" — use data-nd-set only")
	}

	// Fix 3: theme uses the spec pattern: one active theme <link class="theme">
	// that the runtime swaps, PLUS nd-theme metas declaring both available themes.
	if !strings.Contains(body, `class="theme" data-theme="light"`) {
		t.Error("expected an active default theme <link class=\"theme\" data-theme=\"light\"> so a theme stylesheet loads by default")
	}
	if !strings.Contains(body, `themes/light.min.css`) || !strings.Contains(body, `themes/dark.min.css`) {
		t.Error("expected both light and dark theme stylesheet URLs to be referenced")
	}
	// Two nd-theme metas with data-href must declare the available themes.
	if !strings.Contains(body, `name="nd-theme" content="light" data-href=`) {
		t.Error("expected nd-theme light meta with data-href for theme-swap pattern")
	}
	if !strings.Contains(body, `name="nd-theme" content="dark"`) {
		t.Error("expected nd-theme dark meta for theme-swap pattern")
	}
	// The earlier mistake (loading BOTH themes via always-on data-nd-theme-href
	// links, which conflict) must not return.
	if strings.Contains(body, `data-nd-theme-href`) {
		t.Error("must not use conflicting always-on <link data-nd-theme-href> tags")
	}

	if strings.Contains(body, `id="project-detail"`) {
		t.Error("dashboard must not render stale selected-project detail panels")
	}
}

func TestDashboardTemplateUsesThemeCookie(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:16384"
	req.AddCookie(&http.Cookie{Name: themeCookieName, Value: darkTheme})
	rec := httptest.NewRecorder()

	indexHandler(Config{}, store).ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `themes/dark.min.css" class="theme" data-theme="dark"`) {
		t.Error("expected active dark theme stylesheet from cookie")
	}
	if !strings.Contains(body, `eventicPersistTheme`) {
		t.Error("expected theme persistence script")
	}
}

func TestConfigurationTemplateUsesThemeCookie(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/global", nil)
	req.Host = "localhost:16384"
	req.AddCookie(&http.Cookie{Name: themeCookieName, Value: darkTheme})
	rec := httptest.NewRecorder()

	configurationHandler(store).ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `themes/dark.min.css" class="theme" data-theme="dark"`) {
		t.Error("expected active dark theme stylesheet from cookie")
	}
	if !strings.Contains(body, `eventicPersistTheme`) {
		t.Error("expected theme persistence script")
	}
}

func TestDashboardTemplateWSMetaIsAbsolute(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	indexHandler(Config{}, store).ServeHTTP(rec, req)

	body := rec.Body.String()

	// The runs-ws meta content must use an absolute ws:// or wss:// URL.
	if !strings.Contains(body, `content="ws://`) && !strings.Contains(body, `content="wss://`) {
		t.Error("expected runs-ws endpoint meta to be an absolute ws:// or wss:// URL")
	}
}

func TestDashboardTemplateNdesignRowTemplateLiteralBraces(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	indexHandler(Config{}, store).ServeHTTP(rec, req)

	body := rec.Body.String()

	// The ndesign event-row template must contain literal {{field}} syntax —
	// these are ndesign row-template placeholders, not Go template expressions.
	// The template uses [[ ]] as Go delimiters so {{field}} passes through.
	if !strings.Contains(body, "{{repo}}") {
		t.Error("expected literal {{repo}} ndesign row template field in rendered output")
	}
	if !strings.Contains(body, "{{state}}") {
		t.Error("expected literal {{state}} ndesign row template field in rendered output")
	}
	if !strings.Contains(body, "{{delivery_id}}") {
		t.Error("expected literal {{delivery_id}} ndesign row template field in rendered output")
	}
}

func TestDashboardTemplateNoForbiddenMarkup(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	indexHandler(Config{}, store).ServeHTTP(rec, req)

	body := rec.Body.String()

	// No <style> blocks.
	if strings.Contains(body, "<style") {
		t.Error("rendered output must not contain <style> blocks")
	}

	// No inline style= attributes.
	if strings.Contains(body, " style=") {
		t.Error("rendered output must not contain inline style= attributes")
	}

	// The ndesign loader and the small Eventic theme persistence hook are the
	// only dashboard scripts.
	scriptCount := strings.Count(body, "<script")
	if scriptCount != 2 {
		t.Errorf("expected exactly 2 script tags, got %d", scriptCount)
	}
	if !strings.Contains(body, "ndesign.min.js") {
		t.Error("expected ndesign.min.js loader script in rendered output")
	}
	if !strings.Contains(body, "eventicPersistTheme") {
		t.Error("expected Eventic theme persistence script in rendered output")
	}
}

func TestDashboardTemplateNilStoreRendersShell(t *testing.T) {
	// A nil store must still render a valid page shell (empty nav).
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	indexHandler(Config{}, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with nil store, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="app-page"`) {
		t.Error("expected app-page skeleton even with nil store")
	}
}

func TestDashboardTemplateWSBaseUsesHTTPS(t *testing.T) {
	// X-Forwarded-Proto: https must yield wss:// in the meta endpoint.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "eventic.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()

	indexHandler(Config{}, nil).ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `content="wss://`) {
		t.Error("expected wss:// when X-Forwarded-Proto is https")
	}
}
