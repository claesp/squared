package web

import (
	"net/http"
	"strconv"
	"strings"

	"squared/internal/catalog"
	"squared/internal/core"
	"squared/internal/domain"
)

func deploymentURL(slug, env, name string) string {
	return "/tenants/" + slug + "/deployments/" + env + "/" + name
}

func (s *Server) choosePackagePage(w http.ResponseWriter, r *http.Request) error {
	a, err := s.svc.Tenant(r.Context(), userFrom(r), r.PathValue("tenant"))
	if err != nil {
		return err
	}
	if !a.Can(domain.RoleDeveloper) {
		return domain.ErrForbidden
	}
	return s.render(w, r, "choose_package", page{Title: "New deployment", Nav: "tenants", Access: &a, Tab: "overview", Data: s.svc.Catalog().Latest()})
}

// formData is the data of the deployment form.
type formData struct {
	Package      *catalog.Package
	Versions     []*catalog.Package
	Environments []domain.Environment
	Registry     string
	Deployment   *domain.Deployment // nil for a new deployment
	Action       string
}

// packageFormValues returns the form fields for parameter values.
func packageFormValues(pkg *catalog.Package, values map[string]any) map[string]string {
	form := map[string]string{}
	for _, prm := range pkg.Parameters {
		v, ok := values[prm.Name]
		if !ok {
			v = prm.Default
		}
		form[prm.Name] = catalog.FormValue(v)
	}
	return form
}

func (s *Server) deploymentForm(r *http.Request, a core.Access, pkgName, version string) (formData, error) {
	pkg, ok := s.svc.Catalog().Get(pkgName, version)
	if !ok {
		return formData{}, domain.ErrNotFound
	}
	envs, err := s.svc.ListEnvironments(r.Context(), userFrom(r), a.Tenant.Slug)
	if err != nil {
		return formData{}, err
	}
	return formData{
		Package:      pkg,
		Versions:     s.svc.Catalog().Versions(pkgName),
		Environments: envs,
		Registry:     s.cfg.RegistryHost + "/" + s.cfg.ProjectPrefix + a.Tenant.Slug + "/",
	}, nil
}

func (s *Server) newDeploymentPage(w http.ResponseWriter, r *http.Request) error {
	a, err := s.svc.Tenant(r.Context(), userFrom(r), r.PathValue("tenant"))
	if err != nil {
		return err
	}
	if !a.Can(domain.RoleDeveloper) {
		return domain.ErrForbidden
	}
	data, err := s.deploymentForm(r, a, r.PathValue("package"), r.URL.Query().Get("version"))
	if err != nil {
		return err
	}
	data.Action = r.URL.Path
	form := packageFormValues(data.Package, nil)
	form["version"] = data.Package.Version
	return s.render(w, r, "deployment_form", page{Title: "New " + data.Package.Title, Nav: "tenants", Access: &a, Tab: "overview", Data: data, Form: form})
}

func paramForm(r *http.Request, pkg *catalog.Package) map[string]string {
	form := map[string]string{}
	for _, prm := range pkg.Parameters {
		form[prm.Name] = r.PostFormValue(prm.Name)
	}
	return form
}

func (s *Server) createDeployment(w http.ResponseWriter, r *http.Request) error {
	u := userFrom(r)
	a, err := s.svc.Tenant(r.Context(), u, r.PathValue("tenant"))
	if err != nil {
		return err
	}
	data, err := s.deploymentForm(r, a, r.PathValue("package"), r.PostFormValue("version"))
	if err != nil {
		return err
	}
	data.Action = r.URL.Path
	p := page{Title: "New " + data.Package.Title, Nav: "tenants", Access: &a, Tab: "overview", Data: data}

	values, err := data.Package.ParseForm(paramForm(r, data.Package))
	if err != nil {
		return s.invalid(w, r, err, "deployment_form", p)
	}
	env := r.PostFormValue("environment")
	d, err := s.svc.CreateDeployment(r.Context(), u, a.Tenant.Slug, core.DeploymentInput{
		Environment: env,
		Name:        r.PostFormValue("name"),
		Package:     data.Package.Name,
		Version:     data.Package.Version,
		Values:      values,
		Message:     r.PostFormValue("message"),
	})
	if err != nil {
		return s.invalid(w, r, err, "deployment_form", p)
	}
	return s.redirect(w, r, deploymentURL(a.Tenant.Slug, d.Environment, d.Name), "The deployment is saved. The worker now deploys revision 1.")
}

type deploymentData struct {
	View      core.DeploymentView
	Current   *domain.Revision
	Package   *catalog.Package
	CommitURL string // Link to the commit of the current revision, or "".
	RepoURL   string
	Versions  []*catalog.Package
}

func (s *Server) deploymentPage(w http.ResponseWriter, r *http.Request) error {
	u := userFrom(r)
	slug, env, name := r.PathValue("tenant"), r.PathValue("env"), r.PathValue("name")
	a, err := s.svc.Tenant(r.Context(), u, slug)
	if err != nil {
		return err
	}
	view, err := s.svc.Deployment(r.Context(), u, slug, env, name)
	if err != nil {
		return err
	}
	data := deploymentData{View: view, Current: view.Current(), Versions: s.svc.Catalog().Versions(view.Deployment.PackageName)}
	if data.Current != nil {
		if data.Package, err = s.svc.PackageOf(data.Current); err != nil {
			return err
		}
	}
	if s.cfg.GitURL != "" {
		data.RepoURL = s.cfg.GitURL + "/" + s.cfg.GitOrg + "/tenant-" + slug + "/src/branch/main/" + env + "/" + name
		if data.Current != nil && data.Current.CommitSHA != "" {
			data.CommitURL = s.cfg.GitURL + "/" + s.cfg.GitOrg + "/tenant-" + slug + "/commit/" + data.Current.CommitSHA
		}
	}
	refresh := 0
	if !view.Deployment.Status.Done() {
		refresh = 5
	}
	return s.render(w, r, "deployment", page{Title: view.Deployment.Name, Nav: "tenants", Access: &a, Tab: "overview", Data: data, Refresh: refresh})
}

func (s *Server) editDeploymentPage(w http.ResponseWriter, r *http.Request) error {
	u := userFrom(r)
	slug, env, name := r.PathValue("tenant"), r.PathValue("env"), r.PathValue("name")
	a, err := s.svc.Tenant(r.Context(), u, slug)
	if err != nil {
		return err
	}
	if !a.Can(domain.RoleDeveloper) {
		return domain.ErrForbidden
	}
	view, err := s.svc.Deployment(r.Context(), u, slug, env, name)
	if err != nil {
		return err
	}
	version := r.URL.Query().Get("version")
	cur := view.Current()
	if version == "" && cur != nil {
		version = cur.PackageVersion
	}
	data, err := s.deploymentForm(r, a, view.Deployment.PackageName, version)
	if err != nil {
		return err
	}
	data.Deployment = view.Deployment
	data.Action = r.URL.Path
	var values map[string]any
	if cur != nil {
		values = cur.Values
	}
	form := packageFormValues(data.Package, values)
	form["version"] = data.Package.Version
	return s.render(w, r, "deployment_form", page{Title: "Change " + name, Nav: "tenants", Access: &a, Tab: "overview", Data: data, Form: form})
}

func (s *Server) updateDeployment(w http.ResponseWriter, r *http.Request) error {
	u := userFrom(r)
	slug, env, name := r.PathValue("tenant"), r.PathValue("env"), r.PathValue("name")
	a, err := s.svc.Tenant(r.Context(), u, slug)
	if err != nil {
		return err
	}
	view, err := s.svc.Deployment(r.Context(), u, slug, env, name)
	if err != nil {
		return err
	}
	data, err := s.deploymentForm(r, a, view.Deployment.PackageName, r.PostFormValue("version"))
	if err != nil {
		return err
	}
	data.Deployment = view.Deployment
	data.Action = r.URL.Path
	p := page{Title: "Change " + name, Nav: "tenants", Access: &a, Tab: "overview", Data: data}

	values, err := data.Package.ParseForm(paramForm(r, data.Package))
	if err != nil {
		return s.invalid(w, r, err, "deployment_form", p)
	}
	rev, err := s.svc.UpdateDeployment(r.Context(), u, slug, env, name, core.DeploymentInput{
		Version: data.Package.Version,
		Values:  values,
		Message: r.PostFormValue("message"),
	})
	if err != nil {
		return s.invalid(w, r, err, "deployment_form", p)
	}
	return s.redirect(w, r, deploymentURL(slug, env, name), "Revision "+strconv.Itoa(rev.Number)+" is saved. The worker now deploys it.")
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) error {
	slug, env, name := r.PathValue("tenant"), r.PathValue("env"), r.PathValue("name")
	n, err := strconv.Atoi(r.PostFormValue("revision"))
	if err != nil {
		return domain.Invalid("revision", "must be a number")
	}
	rev, err := s.svc.Rollback(r.Context(), userFrom(r), slug, env, name, n)
	if err != nil {
		return err
	}
	return s.redirect(w, r, deploymentURL(slug, env, name), "Revision "+strconv.Itoa(rev.Number)+" is a copy of revision "+strconv.Itoa(n)+". The worker now deploys it.")
}

func (s *Server) syncDeployment(w http.ResponseWriter, r *http.Request) error {
	slug, env, name := r.PathValue("tenant"), r.PathValue("env"), r.PathValue("name")
	if err := s.svc.SyncDeployment(r.Context(), userFrom(r), slug, env, name); err != nil {
		return err
	}
	return s.redirect(w, r, deploymentURL(slug, env, name), "The worker processes the deployment again.")
}

func (s *Server) deleteDeployment(w http.ResponseWriter, r *http.Request) error {
	slug, env, name := r.PathValue("tenant"), r.PathValue("env"), r.PathValue("name")
	if strings.TrimSpace(r.PostFormValue("confirm")) != name {
		return domain.Invalid("confirm", "type the name of the deployment to confirm the removal")
	}
	if err := s.svc.DeleteDeployment(r.Context(), userFrom(r), slug, env, name); err != nil {
		return err
	}
	return s.redirect(w, r, deploymentURL(slug, env, name), "The deployment is being removed.")
}
