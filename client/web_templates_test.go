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

	// nd-nav-section for Global group and each project.
	for _, section := range []string{"Global", "nitecon/alpha", "nitecon/beta"} {
		if !strings.Contains(body, section) {
			t.Errorf("expected nav section %q in rendered output", section)
		}
	}

	// nd-nav-section class itself must appear.
	if !strings.Contains(body, `class="nd-nav-section"`) {
		t.Error("expected nd-nav-section class in rendered output")
	}
}

func TestDashboardTemplateStepNames(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	indexHandler(Config{}, store).ServeHTTP(rec, req)

	body := rec.Body.String()

	// Step names from all workflows must appear.
	for _, stepName := range []string{"Lint", "Test", "Deploy", "Check"} {
		if !strings.Contains(body, stepName) {
			t.Errorf("expected step name %q in rendered nav", stepName)
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
	for _, id := range []string{`id="project-detail"`, `id="project-runs"`, `id="event-stream"`} {
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
		`data-nd-bind="/api/projects/${repo}"`,
		`data-nd-defer`,
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

func TestDashboardTemplateConformance(t *testing.T) {
	store := seedTemplateStore(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:16384"
	rec := httptest.NewRecorder()

	indexHandler(Config{}, store).ServeHTTP(rec, req)

	body := rec.Body.String()

	// Fix 1: runs binding must live on <tbody>, not on the section or table.
	// Confirm the tbody carries the bind and no stray bind remains on the section.
	if !strings.Contains(body, `<tbody data-nd-bind="/api/runs?repo=${repo}&amp;limit=20"`) {
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

	// Fix 3: theme uses the spec pattern — ONE active theme <link class="theme">
	// (the default) that the runtime swaps, PLUS nd-theme metas declaring both.
	// The active default-theme stylesheet must actually load so the page is
	// styled before/without JS, not left unstyled until the first toggle.
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

	// Fix 4: project-detail binding must be on the inner body div, not the section.
	// The inner container carries the bind; the section header is preserved.
	if !strings.Contains(body, `id="project-detail-body"`) {
		t.Error("expected #project-detail-body inner container for the project bind")
	}
	// The section element itself must not carry data-nd-bind (which would wipe the header).
	if strings.Contains(body, `id="project-detail"`+"\n"+"                 data-nd-bind") ||
		strings.Contains(body, `id="project-detail" data-nd-bind`) {
		t.Error("section#project-detail must not carry data-nd-bind — bind lives on inner div")
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

	// Only one <script> tag (the ndesign loader); no extra script blocks.
	scriptCount := strings.Count(body, "<script")
	if scriptCount > 1 {
		t.Errorf("expected exactly 1 <script> tag (ndesign loader), got %d", scriptCount)
	}
	if !strings.Contains(body, "ndesign.min.js") {
		t.Error("expected ndesign.min.js loader script in rendered output")
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
