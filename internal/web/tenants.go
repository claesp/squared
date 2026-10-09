package web

import (
	"net/http"
	"strconv"
	"strings"

	"squared/internal/core"
	"squared/internal/domain"
)

func tenantURL(slug string) string { return "/tenants/" + slug }

func (s *Server) newTenantPage(w http.ResponseWriter, r *http.Request) error {
	if u := userFrom(r); !u.Admin {
		return domain.ErrForbidden
	}
	q := s.svc.DefaultQuota()
	form := map[string]string{
		"environments": "dev, prod",
		"quota_cpu":    q.CPU,
		"quota_memory": q.Memory,
		"quota_pods":   strconv.Itoa(q.Pods),
	}
	return s.render(w, r, "tenant_new", page{Title: "New tenant", Nav: "admin", Form: form})
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func quotaFrom(r *http.Request) domain.Quota {
	pods, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("quota_pods")))
	return domain.Quota{
		CPU:    strings.TrimSpace(r.PostFormValue("quota_cpu")),
		Memory: strings.TrimSpace(r.PostFormValue("quota_memory")),
		Pods:   pods,
	}
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) error {
	t, err := s.svc.CreateTenant(r.Context(), userFrom(r), core.NewTenant{
		Slug:         r.PostFormValue("slug"),
		Name:         r.PostFormValue("name"),
		OwnerEmail:   r.PostFormValue("owner"),
		Environments: splitList(r.PostFormValue("environments")),
		Quota:        quotaFrom(r),
	})
	if err != nil {
		return s.invalid(w, r, err, "tenant_new", page{Title: "New tenant", Nav: "admin"})
	}
	return s.redirect(w, r, tenantURL(t.Slug), "The tenant is added. The worker now prepares it.")
}

type tenantData struct {
	Environments []domain.Environment
	Deployments  []domain.Deployment
	Registry     string
	RepoURL      string
}

func (s *Server) tenantPage(w http.ResponseWriter, r *http.Request) error {
	return s.showTenant(w, r, nil)
}

func (s *Server) showTenant(w http.ResponseWriter, r *http.Request, formErr error) error {
	u := userFrom(r)
	a, err := s.svc.Tenant(r.Context(), u, r.PathValue("tenant"))
	if err != nil {
		return err
	}
	envs, err := s.svc.ListEnvironments(r.Context(), u, a.Tenant.Slug)
	if err != nil {
		return err
	}
	deps, err := s.svc.ListDeployments(r.Context(), u, a.Tenant.Slug)
	if err != nil {
		return err
	}
	data := tenantData{
		Environments: envs,
		Deployments:  deps,
		Registry:     s.cfg.RegistryHost + "/" + s.cfg.ProjectPrefix + a.Tenant.Slug,
	}
	if s.cfg.GitURL != "" {
		data.RepoURL = s.cfg.GitURL + "/" + s.cfg.GitOrg + "/tenant-" + a.Tenant.Slug
	}
	refresh := 0
	if !a.Tenant.Status.Done() {
		refresh = 5
	}
	for _, e := range envs {
		if !e.Status.Done() {
			refresh = 5
		}
	}
	for _, d := range deps {
		if !d.Status.Done() {
			refresh = 5
		}
	}
	p := page{Title: a.Tenant.Name, Nav: "tenants", Access: &a, Tab: "overview", Data: data, Refresh: refresh}
	if formErr != nil {
		p.Refresh = 0
		return s.invalid(w, r, formErr, "tenant", p)
	}
	return s.render(w, r, "tenant", p)
}

func (s *Server) syncTenant(w http.ResponseWriter, r *http.Request) error {
	slug := r.PathValue("tenant")
	if err := s.svc.SyncTenant(r.Context(), userFrom(r), slug); err != nil {
		return err
	}
	return s.redirect(w, r, tenantURL(slug), "The worker prepares the tenant again.")
}

func (s *Server) tenantSettingsPage(w http.ResponseWriter, r *http.Request) error {
	u := userFrom(r)
	if !u.Admin {
		return domain.ErrForbidden
	}
	a, err := s.svc.Tenant(r.Context(), u, r.PathValue("tenant"))
	if err != nil {
		return err
	}
	form := map[string]string{
		"name":         a.Tenant.Name,
		"quota_cpu":    a.Tenant.Quota.CPU,
		"quota_memory": a.Tenant.Quota.Memory,
		"quota_pods":   strconv.Itoa(a.Tenant.Quota.Pods),
	}
	return s.render(w, r, "tenant_settings", page{Title: "Settings", Nav: "tenants", Access: &a, Tab: "settings", Form: form})
}

func (s *Server) updateTenant(w http.ResponseWriter, r *http.Request) error {
	u := userFrom(r)
	slug := r.PathValue("tenant")
	err := s.svc.UpdateTenant(r.Context(), u, slug, r.PostFormValue("name"), quotaFrom(r))
	if err != nil {
		a, aerr := s.svc.Tenant(r.Context(), u, slug)
		if aerr != nil {
			return aerr
		}
		return s.invalid(w, r, err, "tenant_settings", page{Title: "Settings", Nav: "tenants", Access: &a, Tab: "settings"})
	}
	return s.redirect(w, r, tenantURL(slug), "The settings are saved.")
}

func (s *Server) membersPage(w http.ResponseWriter, r *http.Request) error {
	return s.showMembers(w, r, nil)
}

func (s *Server) showMembers(w http.ResponseWriter, r *http.Request, formErr error) error {
	u := userFrom(r)
	a, err := s.svc.Tenant(r.Context(), u, r.PathValue("tenant"))
	if err != nil {
		return err
	}
	members, err := s.svc.ListMembers(r.Context(), u, a.Tenant.Slug)
	if err != nil {
		return err
	}
	p := page{Title: "Members", Nav: "tenants", Access: &a, Tab: "members", Data: members}
	if formErr != nil {
		return s.invalid(w, r, formErr, "members", p)
	}
	return s.render(w, r, "members", p)
}

func (s *Server) putMember(w http.ResponseWriter, r *http.Request) error {
	slug := r.PathValue("tenant")
	err := s.svc.PutMember(r.Context(), userFrom(r), slug, r.PostFormValue("email"), domain.Role(r.PostFormValue("role")))
	if err != nil {
		return s.showMembers(w, r, err)
	}
	return s.redirect(w, r, tenantURL(slug)+"/members", "The member is saved.")
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) error {
	slug := r.PathValue("tenant")
	if err := s.svc.RemoveMember(r.Context(), userFrom(r), slug, r.PathValue("user")); err != nil {
		return s.showMembers(w, r, err)
	}
	return s.redirect(w, r, tenantURL(slug)+"/members", "The member is removed.")
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) error {
	slug := r.PathValue("tenant")
	if _, err := s.svc.CreateEnvironment(r.Context(), userFrom(r), slug, r.PostFormValue("name")); err != nil {
		return s.showTenant(w, r, err)
	}
	return s.redirect(w, r, tenantURL(slug), "The environment is added. The worker now prepares the namespace.")
}

func (s *Server) deleteEnvironment(w http.ResponseWriter, r *http.Request) error {
	slug := r.PathValue("tenant")
	if r.PostFormValue("confirm") != r.PathValue("env") {
		return s.showTenant(w, r, domain.Invalid("confirm", "type the name of the environment to confirm the removal"))
	}
	if err := s.svc.DeleteEnvironment(r.Context(), userFrom(r), slug, r.PathValue("env")); err != nil {
		return s.showTenant(w, r, err)
	}
	return s.redirect(w, r, tenantURL(slug), "The environment is removed. An administrator removes the empty namespace.")
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request) error {
	u := userFrom(r)
	a, err := s.svc.Tenant(r.Context(), u, r.PathValue("tenant"))
	if err != nil {
		return err
	}
	events, err := s.svc.ListAudit(r.Context(), u, a.Tenant.Slug, 200)
	if err != nil {
		return err
	}
	return s.render(w, r, "audit", page{Title: "Audit log", Nav: "tenants", Access: &a, Tab: "audit", Data: events})
}
