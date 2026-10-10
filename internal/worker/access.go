package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"squared/internal/domain"
	"squared/internal/gitops"
	"squared/internal/harbor"
	"squared/internal/keycloak"
	"squared/internal/rest"
)

// EnableAccessSync makes the worker copy the members of each tenant to
// Keycloak groups, Harbor project members and a Forgejo team. Without it,
// the worker does not change access in these systems.
func (w *Worker) EnableAccessSync(kc *keycloak.Client) { w.keycloak = kc }

// harborRoles maps the roles of a tenant to Harbor project roles. Owners
// get "maintainer", not "project admin": Squared manages the members, thus
// nobody changes them in Harbor.
var harborRoles = map[domain.Role]int{
	domain.RoleOwner:     harbor.RoleMaintainer,
	domain.RoleDeveloper: harbor.RoleDeveloper,
	domain.RoleViewer:    harbor.RoleGuest,
}

// teamName returns the name of the Forgejo team of a tenant.
func (w *Worker) teamName(slug string) string {
	prefix := w.cfg.GitOps.GroupPrefix
	if prefix == "" {
		prefix = "tenant-"
	}
	return prefix + slug
}

// accessMember is a member of a tenant who has access.
type accessMember struct {
	user *domain.User
	role domain.Role
}

// syncAccess makes the access in Keycloak, Harbor and Forgejo agree with
// the members of a tenant:
//
//   - Keycloak: one group for each role, with the members of the role.
//   - Harbor: the three groups are members of the project of the tenant.
//   - Forgejo: one team with read access to the tenant repository.
//
// Squared is the source of the members. The worker removes members that
// somebody added by hand in these systems. Disabled users lose access.
func (w *Worker) syncAccess(ctx context.Context, t *domain.Tenant) error {
	if w.keycloak == nil {
		return nil
	}
	memberships, err := w.store.ListMemberships(ctx, t.ID)
	if err != nil {
		return err
	}
	var members []accessMember
	for _, m := range memberships {
		u, err := w.store.UserByID(ctx, m.UserID)
		if err != nil {
			return err
		}
		if !u.Disabled {
			members = append(members, accessMember{user: u, role: m.Role})
		}
	}
	if err := w.syncKeycloakGroups(ctx, t, members); err != nil {
		return fmt.Errorf("keycloak: %w", err)
	}
	if err := w.syncHarborMembers(ctx, t); err != nil {
		return fmt.Errorf("harbor: %w", err)
	}
	if err := w.syncForgejoTeam(ctx, t, members); err != nil {
		return fmt.Errorf("forgejo: %w", err)
	}
	return nil
}

func (w *Worker) syncKeycloakGroups(ctx context.Context, t *domain.Tenant, members []accessMember) error {
	for _, role := range domain.Roles {
		g, err := w.keycloak.EnsureGroup(ctx, w.cfg.GitOps.TenantGroup(t.Slug, role))
		if err != nil {
			return err
		}
		want := map[string]bool{}
		for _, m := range members {
			if m.role != role {
				continue
			}
			// A user who signed in with Keycloak has the Keycloak user ID
			// as subject. For other users, Keycloak finds the user by the
			// email address. A user without a Keycloak account gets the
			// access later, when the account exists.
			id := m.user.OIDCSubject
			if id == "" {
				id, err = w.keycloak.UserIDByEmail(ctx, m.user.Email)
				if errors.Is(err, keycloak.ErrUserNotFound) {
					continue
				}
				if err != nil {
					return err
				}
			}
			want[id] = true
		}
		have, err := w.keycloak.Members(ctx, g.ID)
		if err != nil {
			return err
		}
		has := map[string]bool{}
		for _, id := range have {
			has[id] = true
			if !want[id] {
				if err := w.keycloak.RemoveMember(ctx, g.ID, id); err != nil {
					return err
				}
			}
		}
		for id := range want {
			if !has[id] {
				if err := w.keycloak.AddMember(ctx, g.ID, id); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (w *Worker) syncHarborMembers(ctx context.Context, t *domain.Tenant) error {
	project := w.HarborProject(t.Slug)
	members, err := w.harbor.Members(ctx, project)
	if err != nil {
		return err
	}
	// Harbor compares the group names with the values of the groups claim,
	// which are full paths.
	want := map[string]int{}
	for role, harborRole := range harborRoles {
		want[w.cfg.GitOps.TenantGroupClaim(t.Slug, role)] = harborRole
	}
	found := map[string]bool{}
	prefix := "/" + w.teamName(t.Slug) + "-"
	for _, m := range members {
		if m.EntityType != "g" {
			continue // Users, for example the admin who made the project.
		}
		role, ok := want[m.EntityName]
		switch {
		case ok && m.RoleID != role:
			if err := w.harbor.SetMemberRole(ctx, project, m.ID, role); err != nil {
				return err
			}
		case !ok && strings.HasPrefix(m.EntityName, prefix):
			if err := w.harbor.RemoveMember(ctx, project, m.ID); err != nil {
				return err
			}
		}
		found[m.EntityName] = true
	}
	for group, role := range want {
		if !found[group] {
			if err := w.harbor.AddGroupMember(ctx, project, group, role); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *Worker) syncForgejoTeam(ctx context.Context, t *domain.Tenant, members []accessMember) error {
	g := w.cfg.GitOps
	team, err := w.forgejo.EnsureTeam(ctx, g.Org, w.teamName(t.Slug), "Members of tenant "+t.Slug+". Read access to the tenant repository. Squared manages the members.")
	if err != nil {
		return err
	}
	if err := w.forgejo.AddTeamRepo(ctx, team.ID, g.Org, gitops.TenantRepo(t.Slug)); err != nil {
		return err
	}
	want := map[string]bool{}
	for _, m := range members {
		name, err := w.forgejo.EnsureUser(ctx, m.user.Email, m.user.Name)
		if err != nil {
			return err
		}
		want[name] = true
	}
	have, err := w.forgejo.TeamMembers(ctx, team.ID)
	if err != nil {
		return err
	}
	has := map[string]bool{}
	for _, name := range have {
		has[name] = true
		if !want[name] {
			if err := w.forgejo.RemoveTeamMember(ctx, team.ID, name); err != nil {
				return err
			}
		}
	}
	for name := range want {
		if !has[name] {
			if err := w.forgejo.AddTeamMember(ctx, team.ID, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeAccess removes the groups, the Harbor group members and the
// Forgejo team of a tenant.
func (w *Worker) removeAccess(ctx context.Context, t *domain.Tenant) error {
	if w.keycloak == nil {
		return nil
	}
	members, err := w.harbor.Members(ctx, w.HarborProject(t.Slug))
	if err != nil && !rest.HasStatus(err, http.StatusNotFound) {
		return fmt.Errorf("harbor: %w", err)
	}
	prefix := "/" + w.teamName(t.Slug) + "-"
	for _, m := range members {
		if m.EntityType == "g" && strings.HasPrefix(m.EntityName, prefix) {
			if err := w.harbor.RemoveMember(ctx, w.HarborProject(t.Slug), m.ID); err != nil {
				return fmt.Errorf("harbor: %w", err)
			}
		}
	}
	for _, role := range domain.Roles {
		if err := w.keycloak.DeleteGroup(ctx, w.cfg.GitOps.TenantGroup(t.Slug, role)); err != nil {
			return fmt.Errorf("keycloak: %w", err)
		}
	}
	if err := w.forgejo.DeleteTeam(ctx, w.cfg.GitOps.Org, w.teamName(t.Slug)); err != nil {
		return fmt.Errorf("forgejo: %w", err)
	}
	return nil
}

// syncTenantAccess processes a tenant.access task. A tenant that is not
// ready gets its access with the next tenant sync.
func (w *Worker) syncTenantAccess(ctx context.Context, tenantID string) error {
	t, err := w.store.TenantByID(ctx, tenantID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if t.Status != domain.StatusReady {
		return nil
	}
	return w.syncAccess(ctx, t)
}
