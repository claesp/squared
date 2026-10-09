package api

import (
	"net/http"

	"squared/internal/core"
	"squared/internal/domain"
)

func (s *Server) me(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	writeJSON(w, http.StatusOK, userOf(u))
	return nil
}

func (s *Server) listPackages(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	pkgs := s.svc.Catalog().Latest()
	out := make([]PackageSummary, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, summaryOf(p))
	}
	writeJSON(w, http.StatusOK, listOf(out))
	return nil
}

func (s *Server) packageVersions(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	versions := s.svc.Catalog().Versions(r.PathValue("name"))
	if len(versions) == 0 {
		return domain.ErrNotFound
	}
	out := make([]PackageSummary, 0, len(versions))
	for _, p := range versions {
		out = append(out, summaryOf(p))
	}
	writeJSON(w, http.StatusOK, listOf(out))
	return nil
}

// getPackage returns the full package definition, as in the catalog file.
func (s *Server) getPackage(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	p, ok := s.svc.Catalog().Get(r.PathValue("name"), r.PathValue("version"))
	if !ok {
		return domain.ErrNotFound
	}
	writeJSON(w, http.StatusOK, p)
	return nil
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	users, err := s.svc.ListUsers(r.Context(), u)
	if err != nil {
		return err
	}
	out := make([]User, 0, len(users))
	for i := range users {
		out = append(out, userOf(&users[i]))
	}
	writeJSON(w, http.StatusOK, listOf(out))
	return nil
}

type createUserRequest struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"`
	Admin    bool   `json:"admin"`
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	var in createUserRequest
	if err := decode(r, &in); err != nil {
		return err
	}
	created, err := s.svc.CreateUser(r.Context(), u, core.NewUser{Email: in.Email, Name: in.Name, Password: in.Password, Admin: in.Admin})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, userOf(created))
	return nil
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	tenants, err := s.svc.ListTenants(r.Context(), u)
	if err != nil {
		return err
	}
	out := make([]Tenant, 0, len(tenants))
	for i := range tenants {
		out = append(out, tenantOf(&tenants[i], ""))
	}
	writeJSON(w, http.StatusOK, listOf(out))
	return nil
}

type createTenantRequest struct {
	Slug         string   `json:"slug"`
	Name         string   `json:"name"`
	Owner        string   `json:"owner"`
	Environments []string `json:"environments"`
	Quota        *Quota   `json:"quota"`
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	var in createTenantRequest
	if err := decode(r, &in); err != nil {
		return err
	}
	q := s.svc.DefaultQuota()
	if in.Quota != nil {
		q = domain.Quota{CPU: in.Quota.CPU, Memory: in.Quota.Memory, Pods: in.Quota.Pods}
	}
	t, err := s.svc.CreateTenant(r.Context(), u, core.NewTenant{
		Slug: in.Slug, Name: in.Name, OwnerEmail: in.Owner, Environments: in.Environments, Quota: q,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, tenantOf(t, domain.RoleOwner))
	return nil
}

func (s *Server) getTenant(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	a, err := s.svc.Tenant(r.Context(), u, r.PathValue("tenant"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, tenantOf(a.Tenant, a.Role))
	return nil
}

type updateTenantRequest struct {
	Name  string `json:"name"`
	Quota Quota  `json:"quota"`
}

func (s *Server) updateTenant(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	var in updateTenantRequest
	if err := decode(r, &in); err != nil {
		return err
	}
	slug := r.PathValue("tenant")
	q := domain.Quota{CPU: in.Quota.CPU, Memory: in.Quota.Memory, Pods: in.Quota.Pods}
	if err := s.svc.UpdateTenant(r.Context(), u, slug, in.Name, q); err != nil {
		return err
	}
	return s.getTenant(w, r, u)
}

type deleteTenantRequest struct {
	Confirm string `json:"confirm"`
}

func (s *Server) deleteTenant(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	var in deleteTenantRequest
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.svc.DeleteTenant(r.Context(), u, r.PathValue("tenant"), in.Confirm); err != nil {
		return err
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}

func (s *Server) syncTenant(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	if err := s.svc.SyncTenant(r.Context(), u, r.PathValue("tenant")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	events, err := s.svc.ListAudit(r.Context(), u, r.PathValue("tenant"), 500)
	if err != nil {
		return err
	}
	out := make([]AuditEvent, 0, len(events))
	for _, e := range events {
		out = append(out, AuditEvent{Time: e.CreatedAt, Actor: e.ActorEmail, Action: e.Action, Target: e.Target, Detail: e.Detail})
	}
	writeJSON(w, http.StatusOK, listOf(out))
	return nil
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	members, err := s.svc.ListMembers(r.Context(), u, r.PathValue("tenant"))
	if err != nil {
		return err
	}
	out := make([]Member, 0, len(members))
	for _, m := range members {
		out = append(out, Member{UserID: m.UserID, Email: m.Email, Name: m.Name, Role: string(m.Role), CreatedAt: m.CreatedAt})
	}
	writeJSON(w, http.StatusOK, listOf(out))
	return nil
}

type putMemberRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

func (s *Server) putMember(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	var in putMemberRequest
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.svc.PutMember(r.Context(), u, r.PathValue("tenant"), in.Email, domain.Role(in.Role)); err != nil {
		return err
	}
	return s.listMembers(w, r, u)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	if err := s.svc.RemoveMember(r.Context(), u, r.PathValue("tenant"), r.PathValue("user")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func environmentOf(e *domain.Environment) Environment {
	return Environment{Name: e.Name, Namespace: e.Namespace, Status: string(e.Status), StatusMessage: e.StatusMessage, CreatedAt: e.CreatedAt}
}

func (s *Server) listEnvironments(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	envs, err := s.svc.ListEnvironments(r.Context(), u, r.PathValue("tenant"))
	if err != nil {
		return err
	}
	out := make([]Environment, 0, len(envs))
	for i := range envs {
		out = append(out, environmentOf(&envs[i]))
	}
	writeJSON(w, http.StatusOK, listOf(out))
	return nil
}

type createEnvironmentRequest struct {
	Name string `json:"name"`
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	var in createEnvironmentRequest
	if err := decode(r, &in); err != nil {
		return err
	}
	e, err := s.svc.CreateEnvironment(r.Context(), u, r.PathValue("tenant"), in.Name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, environmentOf(e))
	return nil
}

func (s *Server) deleteEnvironment(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	if err := s.svc.DeleteEnvironment(r.Context(), u, r.PathValue("tenant"), r.PathValue("env")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	deps, err := s.svc.ListDeployments(r.Context(), u, r.PathValue("tenant"))
	if err != nil {
		return err
	}
	out := make([]Deployment, 0, len(deps))
	for i := range deps {
		out = append(out, deploymentOf(&deps[i]))
	}
	writeJSON(w, http.StatusOK, listOf(out))
	return nil
}

type createDeploymentRequest struct {
	Environment string         `json:"environment"`
	Name        string         `json:"name"`
	Package     string         `json:"package"`
	Version     string         `json:"version"`
	Values      map[string]any `json:"values"`
	Message     string         `json:"message"`
}

func (s *Server) createDeployment(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	var in createDeploymentRequest
	if err := decode(r, &in); err != nil {
		return err
	}
	slug := r.PathValue("tenant")
	d, err := s.svc.CreateDeployment(r.Context(), u, slug, core.DeploymentInput{
		Environment: in.Environment, Name: in.Name, Package: in.Package, Version: in.Version,
		Values: in.Values, Message: in.Message,
	})
	if err != nil {
		return err
	}
	view, err := s.svc.Deployment(r.Context(), u, slug, d.Environment, d.Name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, viewOf(view))
	return nil
}

func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	view, err := s.svc.Deployment(r.Context(), u, r.PathValue("tenant"), r.PathValue("env"), r.PathValue("name"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, viewOf(view))
	return nil
}

type updateDeploymentRequest struct {
	Version string         `json:"version"`
	Values  map[string]any `json:"values"`
	Message string         `json:"message"`
}

func (s *Server) updateDeployment(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	var in updateDeploymentRequest
	if err := decode(r, &in); err != nil {
		return err
	}
	slug, env, name := r.PathValue("tenant"), r.PathValue("env"), r.PathValue("name")
	if _, err := s.svc.UpdateDeployment(r.Context(), u, slug, env, name, core.DeploymentInput{
		Version: in.Version, Values: in.Values, Message: in.Message,
	}); err != nil {
		return err
	}
	return s.getDeployment(w, r, u)
}

type rollbackRequest struct {
	Revision int `json:"revision"`
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	var in rollbackRequest
	if err := decode(r, &in); err != nil {
		return err
	}
	slug, env, name := r.PathValue("tenant"), r.PathValue("env"), r.PathValue("name")
	if _, err := s.svc.Rollback(r.Context(), u, slug, env, name, in.Revision); err != nil {
		return err
	}
	return s.getDeployment(w, r, u)
}

func (s *Server) syncDeployment(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	if err := s.svc.SyncDeployment(r.Context(), u, r.PathValue("tenant"), r.PathValue("env"), r.PathValue("name")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}

func (s *Server) deleteDeployment(w http.ResponseWriter, r *http.Request, u *domain.User) error {
	if err := s.svc.DeleteDeployment(r.Context(), u, r.PathValue("tenant"), r.PathValue("env"), r.PathValue("name")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}
