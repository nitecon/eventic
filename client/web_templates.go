package client

import (
	"embed"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

var configurationTmpl = template.Must(
	template.New("configuration.html").
		Delims("[[", "]]").
		ParseFS(templateFS, "templates/configuration.html"),
)

// dashboardView is the server-side view-model rendered into the dashboard
// template for each request. It carries the left-nav hierarchy (built from the
// ProjectStore) and the computed WebSocket base URL.
type dashboardView struct {
	// Brand is the short app name shown in the sidebar and browser tab.
	Brand string
	// Orgs lists repository owners available in the organization selector.
	Orgs []string
	// DefaultOrg is the initially selected organization.
	DefaultOrg string
	// Projects lists initial repositories for DefaultOrg. The browser refreshes
	// this list through /api/projects?summary=1 after ndesign starts.
	Projects []ProjectSummary
	// WSBase is the absolute WebSocket URL prefix, e.g. ws://host/ws/runs.
	// It is injected as an <meta name="endpoint:runs-ws"> so ndesign markup can
	// reference it via ${runs-ws} in data-nd-ws attributes.
	WSBase string
	// APIBase is the base URL for REST calls (empty means same-origin relative).
	APIBase string
}

type configurationView struct {
	Brand     string
	Title     string
	Scope     string
	Repo      string
	Owner     string
	Name      string
	IsGlobal  bool
	Project   *ProjectState
	Workflows []workflowConfig
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

func configurationHandler(store *ProjectStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if store == nil {
			http.Error(w, "project store disabled", http.StatusServiceUnavailable)
			return
		}

		view, ok, err := buildConfigurationView(r, store)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if isNotFound(err) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "configuration error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := configurationTmpl.Execute(w, view); err != nil {
			http.Error(w, "template error", http.StatusInternalServerError)
		}
	})
}

func buildConfigurationView(r *http.Request, store *ProjectStore) (configurationView, bool, error) {
	scope, repo, ok := configurationRoute(r.URL.Path)
	if !ok {
		return configurationView{}, false, nil
	}

	view := configurationView{
		Brand:    "Eventic",
		Scope:    scope,
		Repo:     repo,
		IsGlobal: scope == WorkflowScopeGlobal,
	}
	if view.IsGlobal {
		view.Title = "Global Workflows"
	} else {
		view.Owner, view.Name = splitRepoName(repo)
		view.Title = repo
		project, err := store.GetProject(r.Context(), repo)
		if err != nil {
			return configurationView{}, true, err
		}
		view.Project = project
	}

	workflows, err := listWorkflowConfigs(r.Context(), store, scope, repo)
	if err != nil {
		return configurationView{}, true, err
	}
	view.Workflows = workflows
	return view, true, nil
}

func configurationRoute(path string) (string, string, bool) {
	clean := strings.Trim(path, "/")
	if clean == "global" {
		return WorkflowScopeGlobal, "", true
	}

	parts := strings.Split(clean, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return WorkflowScopeRepo, parts[0] + "/" + parts[1], true
}

// buildDashboardView constructs the dashboardView from the store and the
// incoming request. It is nil-safe: a nil store yields an empty nav while
// the page shell still renders correctly.
func buildDashboardView(r *http.Request, store *ProjectStore) dashboardView {
	ctx := r.Context()

	view := dashboardView{
		Brand:      "Eventic",
		DefaultOrg: "nitecon",
		WSBase:     wsBase(r),
		APIBase:    "",
	}

	if store == nil {
		return view
	}

	allProjects, _ := store.ListProjectSummaries(ctx, "", "")
	view.Orgs = projectOrgs(allProjects)
	if !containsString(view.Orgs, view.DefaultOrg) && len(view.Orgs) > 0 {
		view.DefaultOrg = view.Orgs[0]
	}
	if view.DefaultOrg != "" {
		view.Projects, _ = store.ListProjectSummaries(ctx, view.DefaultOrg, "")
	}

	return view
}

func projectOrgs(projects []ProjectSummary) []string {
	seen := map[string]struct{}{}
	for _, project := range projects {
		if project.Owner == "" {
			continue
		}
		seen[project.Owner] = struct{}{}
	}
	orgs := make([]string, 0, len(seen))
	for org := range seen {
		orgs = append(orgs, org)
	}
	sort.Strings(orgs)
	return orgs
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
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
