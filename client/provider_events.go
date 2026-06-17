package client

import (
	"sort"
	"strings"
)

// ProviderEvent describes a provider-facing event pattern that can be dragged
// onto a stable event target to create a declarative mapping.
type ProviderEvent struct {
	Provider    string            `json:"provider"`
	Event       string            `json:"event"`
	Action      string            `json:"action,omitempty"`
	Label       string            `json:"label"`
	Description string            `json:"description,omitempty"`
	Conditions  map[string]string `json:"conditions"`
}

func ProviderEventCatalog() []ProviderEvent {
	events := []ProviderEvent{
		providerEvent("*", "*", "", "Any provider event", "Catch all inbound events that reach Eventic.", map[string]string{"event": "*"}),

		providerEvent("github", "push", "", "GitHub push", "Any GitHub push event.", map[string]string{"event": "push"}),
		providerEvent("github", "push", "", "GitHub push to main", "Main branch push for standard artifact builds.", map[string]string{"event": "push", "ref": "refs/heads/main"}),
		providerEvent("github", "push", "", "GitHub tag push", "Tag push that can promote an existing artifact.", map[string]string{"event": "push", "ref": "refs/tags/*"}),
		providerEvent("github", "release", "published", "GitHub release published", "Published release used for promotion or validation.", map[string]string{"event": "release", "action": "published"}),
		providerEvent("github", "workflow_run", "completed", "GitHub workflow failed", "Completed workflow with a failure conclusion.", map[string]string{"event": "workflow_run", "action": "completed", "body.workflow_run.conclusion": "failure"}),
		providerEvent("github", "dependabot_alert", "created", "GitHub Dependabot alert", "New dependency alert.", map[string]string{"event": "dependabot_alert", "action": "created"}),
		providerEvent("github", "code_scanning_alert", "created", "GitHub code scanning alert", "New code scanning finding.", map[string]string{"event": "code_scanning_alert", "action": "created"}),
		providerEvent("github", "security_advisory", "published", "GitHub security advisory", "Published security advisory.", map[string]string{"event": "security_advisory", "action": "published"}),
		providerEvent("github", "repository_vulnerability_alert", "create", "GitHub vulnerability alert", "Repository vulnerability alert.", map[string]string{"event": "repository_vulnerability_alert", "action": "create"}),
		providerEvent("github", "deployment_status", "created", "GitHub deployment failed", "Deployment status with a failure state.", map[string]string{"event": "deployment_status", "action": "created", "body.deployment_status.state": "failure,error"}),
		providerEvent("github", "pull_request", "closed", "GitHub PR merged", "Merged pull request.", map[string]string{"event": "pull_request", "action": "closed", "body.pull_request.merged": "true"}),

		providerEvent("gitlab", "push", "", "GitLab push", "Any GitLab push hook.", map[string]string{"event": "push"}),
		providerEvent("gitlab", "push", "", "GitLab push to main", "Main branch push for standard artifact builds.", map[string]string{"event": "push", "ref": "refs/heads/main"}),
		providerEvent("gitlab", "tag_push", "", "GitLab tag push", "Tag push that can promote an existing artifact.", map[string]string{"event": "tag_push", "ref": "refs/tags/*"}),
		providerEvent("gitlab", "merge_request", "merge", "GitLab merge request merged", "Merged merge request.", map[string]string{"event": "merge_request", "action": "merge"}),
		providerEvent("gitlab", "merge_request", "merged", "GitLab merge request state merged", "Merge request whose state is merged.", map[string]string{"event": "merge_request", "action": "merged"}),
		providerEvent("gitlab", "pipeline", "failed", "GitLab pipeline failed", "Pipeline failed for build or deploy remediation.", map[string]string{"event": "pipeline", "action": "failed"}),
		providerEvent("gitlab", "job", "failed", "GitLab job failed", "Failed CI job for build remediation.", map[string]string{"event": "job", "action": "failed"}),
		providerEvent("gitlab", "release", "create", "GitLab release created", "Release creation used for promotion or validation.", map[string]string{"event": "release", "action": "create"}),
		providerEvent("gitlab", "vulnerability", "detected", "GitLab vulnerability detected", "Security vulnerability finding.", map[string]string{"event": "vulnerability", "metadata.severity": "CRITICAL,HIGH"}),

		providerEvent("bitbucket", "repo.push", "", "Bitbucket push", "Any repository push.", map[string]string{"event": "repo.push"}),
		providerEvent("bitbucket", "repo.push", "", "Bitbucket push to main", "Main branch push for standard artifact builds.", map[string]string{"event": "repo.push", "ref": "refs/heads/main"}),
		providerEvent("bitbucket", "repo.push", "", "Bitbucket tag push", "Repository push that includes a tag ref.", map[string]string{"event": "repo.push", "ref": "refs/tags/*"}),
		providerEvent("bitbucket", "pullrequest.fulfilled", "", "Bitbucket PR merged", "Fulfilled pull request.", map[string]string{"event": "pullrequest.fulfilled"}),
		providerEvent("bitbucket", "repo.commit_status_updated", "FAILED", "Bitbucket pipeline failed", "Failed commit status.", map[string]string{"event": "repo.commit_status_updated", "action": "FAILED"}),

		providerEvent("prometheus", "alert", "firing", "Prometheus alert firing", "Alertmanager firing notification.", map[string]string{"event": "alert", "body.status": "firing"}),
		providerEvent("prometheus", "alert", "resolved", "Prometheus alert resolved", "Alertmanager resolved notification.", map[string]string{"event": "alert", "body.status": "resolved"}),
		providerEvent("prometheus", "alert", "critical", "Prometheus critical alert", "Critical severity alert.", map[string]string{"event": "alert", "metadata.severity": "CRITICAL"}),
		providerEvent("prometheus", "alert", "warning", "Prometheus warning alert", "Warning severity alert.", map[string]string{"event": "alert", "metadata.severity": "WARNING"}),

		providerEvent("discord", "application_command", "", "Discord command", "Discord slash-command payload routed through an authenticated adapter.", map[string]string{"event": "application_command"}),
		providerEvent("discord", "message", "", "Discord message", "Discord message-style payload routed through an authenticated adapter.", map[string]string{"event": "message"}),
		providerEvent("discord", "ping", "", "Discord ping", "Discord endpoint verification or adapter ping.", map[string]string{"event": "ping"}),

		providerEvent("custom", "comms", "", "Internal communication", "Internal title/body/action message.", map[string]string{"event": "comms"}),
		providerEvent("custom", "scan", "", "Critical vulnerability scan", "Registry or dependency scan with a critical vulnerability.", map[string]string{"body.vulnerabilities[].severity": "CRITICAL"}),
		providerEvent("custom", "cve", "", "CVE notification", "External CVE intake with a high-risk severity.", map[string]string{"event": "cve", "metadata.severity": "CRITICAL,HIGH"}),
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].Provider == events[j].Provider {
			return events[i].Label < events[j].Label
		}
		if events[i].Provider == "*" {
			return true
		}
		if events[j].Provider == "*" {
			return false
		}
		return events[i].Provider < events[j].Provider
	})
	return events
}

func FilterProviderEvents(provider string) []ProviderEvent {
	provider = strings.ToLower(strings.TrimSpace(provider))
	events := ProviderEventCatalog()
	if provider == "" || provider == "all" {
		return events
	}
	filtered := make([]ProviderEvent, 0, len(events))
	for _, event := range events {
		if strings.EqualFold(event.Provider, provider) {
			filtered = append(filtered, event)
		}
	}
	return filtered
}

func ProviderEventOptions() []selectOption {
	seen := map[string]bool{"all": true}
	options := []selectOption{{Value: "all", Label: "All Providers"}}
	for _, event := range ProviderEventCatalog() {
		provider := strings.TrimSpace(event.Provider)
		if provider == "" || provider == "*" || seen[provider] {
			continue
		}
		seen[provider] = true
		options = append(options, selectOption{Value: provider, Label: provider})
	}
	return options
}

func providerEvent(provider, event, action, label, description string, conditions map[string]string) ProviderEvent {
	return ProviderEvent{
		Provider:    provider,
		Event:       event,
		Action:      action,
		Label:       label,
		Description: description,
		Conditions:  cloneStringMap(conditions),
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range values {
		out[key] = value
	}
	return out
}
