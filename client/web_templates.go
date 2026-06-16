package client

import (
	"embed"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
)

//go:embed templates/*.html
var templateFS embed.FS

// dashboardTmpl is the compiled dashboard template. It uses [[ ]] as Go
// delimiters so that ndesign's own {{field}} row-template syntax passes through
// untouched to the browser (ndesign interprets those at runtime).
var dashboardTmpl = template.Must(
	template.New("dashboard.html").
		Delims("[[", "]]").
		ParseFS(templateFS, "templates/dashboard.html"),
)

// dashboardView is the server-side view-model rendered into the dashboard
// template for each request. It carries the left-nav hierarchy (built from the
// ProjectStore) and the computed WebSocket base URL.
type dashboardView struct {
	// Brand is the short app name shown in the sidebar and browser tab.
	Brand string
	// Global lists workflows with global scope.
	Global []navWorkflow
	// Projects lists per-repo nav groups, each with their workflows.
	Projects []navProject
	// WSBase is the absolute WebSocket URL prefix, e.g. ws://host/ws/runs.
	// It is injected as an <meta name="endpoint:runs-ws"> so ndesign markup can
	// reference it via ${runs-ws} in data-nd-ws attributes.
	WSBase string
	// APIBase is the base URL for REST calls (empty means same-origin relative).
	APIBase string
}

// navProject is one project entry in the left-nav hierarchy.
type navProject struct {
	// Repo is the full "org/repo" identifier.
	Repo string
	// Workflows lists the configured workflows under this project.
	Workflows []navWorkflow
}

// navWorkflow is one workflow entry within a nav group.
type navWorkflow struct {
	// ID is the workflow database id, used to construct sidebar anchors.
	ID int64
	// Name is the human-readable workflow name.
	Name string
	// EventType is the event.action key this workflow responds to.
	EventType string
	// Scope is "global" or "repo".
	Scope string
	// Enabled indicates whether the workflow is currently active.
	Enabled bool
	// Steps lists the workflow's nodes in DAG order (as returned by GetWorkflow).
	Steps []navStep
}

// navStep is one node rendered beneath a workflow in the sidebar.
type navStep struct {
	// Name is the human-readable node name (may be empty for anonymous nodes).
	Name string
	// Command is the shell command, shown as a fallback when Name is empty.
	Command string
}

// indexHandler serves the dashboard at "/". When StaticDir contains an
// index.html it is served via http.FileServer so the user can supply custom
// ndesign markup; otherwise the embedded Go template shell is rendered with
// a view-model built from the store.
//
// Signature update: accepts full Config (not just WebConfig) and a *ProjectStore
// so the server-side left-nav hierarchy can be populated at render time.
func indexHandler(cfg Config, store *ProjectStore) http.Handler {
	if cfg.Web.StaticDir != "" {
		if info, err := os.Stat(filepath.Join(cfg.Web.StaticDir, "index.html")); err == nil && !info.IsDir() {
			return http.FileServer(http.Dir(cfg.Web.StaticDir))
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		view := buildDashboardView(r, store)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := dashboardTmpl.Execute(w, view); err != nil {
			// Headers already sent; log the error and terminate gracefully.
			http.Error(w, "template error", http.StatusInternalServerError)
		}
	})
}

// buildDashboardView constructs the dashboardView from the store and the
// incoming request. It is nil-safe: a nil store yields an empty nav while
// the page shell still renders correctly.
func buildDashboardView(r *http.Request, store *ProjectStore) dashboardView {
	ctx := r.Context()

	view := dashboardView{
		Brand:   "Eventic",
		WSBase:  wsBase(r),
		APIBase: "",
	}

	if store == nil {
		return view
	}

	// ── Global workflows ────────────────────────────────────────────────────
	globalWFs, _ := store.ListWorkflows(ctx, WorkflowScopeGlobal, "", "")
	for _, wf := range globalWFs {
		full, err := store.GetWorkflow(ctx, wf.ID)
		if err != nil || full == nil {
			view.Global = append(view.Global, navWorkflow{
				ID:        wf.ID,
				Name:      wf.Name,
				EventType: wf.EventType,
				Scope:     wf.Scope,
				Enabled:   wf.Enabled,
			})
			continue
		}
		view.Global = append(view.Global, navWorkflowFrom(*full))
	}

	// ── Per-project workflows ────────────────────────────────────────────────
	repos, _ := store.ListProjects(ctx)
	for _, repo := range repos {
		projectWFs, _ := store.ListWorkflows(ctx, WorkflowScopeRepo, repo, "")
		nav := navProject{Repo: repo}
		for _, wf := range projectWFs {
			full, err := store.GetWorkflow(ctx, wf.ID)
			if err != nil || full == nil {
				nav.Workflows = append(nav.Workflows, navWorkflow{
					ID:        wf.ID,
					Name:      wf.Name,
					EventType: wf.EventType,
					Scope:     wf.Scope,
					Enabled:   wf.Enabled,
				})
				continue
			}
			nav.Workflows = append(nav.Workflows, navWorkflowFrom(*full))
		}
		view.Projects = append(view.Projects, nav)
	}

	return view
}

// navWorkflowFrom converts a full Workflow (with nodes) to a navWorkflow.
func navWorkflowFrom(wf Workflow) navWorkflow {
	nav := navWorkflow{
		ID:        wf.ID,
		Name:      wf.Name,
		EventType: wf.EventType,
		Scope:     wf.Scope,
		Enabled:   wf.Enabled,
	}
	for _, node := range wf.Nodes {
		nav.Steps = append(nav.Steps, navStep{
			Name:    node.Name,
			Command: node.Command,
		})
	}
	return nav
}

// wsBase derives the absolute WebSocket URL prefix from the request, e.g.
// "ws://localhost:16384/ws/runs". It honours X-Forwarded-Proto for proxied
// setups and r.TLS for direct TLS.
func wsBase(r *http.Request) string {
	scheme := "ws"
	if r.TLS != nil {
		scheme = "wss"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" {
		scheme = "wss"
	}
	return scheme + "://" + r.Host + "/ws/runs"
}
