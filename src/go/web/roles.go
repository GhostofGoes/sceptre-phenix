package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/activeshadow/structs"
	"github.com/gorilla/mux"

	apiConfig "phenix/api/config"
	"phenix/store"
	"phenix/types"
	v1 "phenix/types/version/v1"
	"phenix/util/plog"
	"phenix/web/middleware"
	"phenix/web/rbac"
	"phenix/web/util"
)

var (
	errRoleNameExists   = errors.New("role name already exists")
	metadataNameCleanup = regexp.MustCompile(`[^a-zA-Z0-9_@.-]+`)
) //nolint:gochecknoglobals // immutable package state

func GetRoleResources(w http.ResponseWriter, r *http.Request) {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "GetRoleResources")

	var (
		ctx     = r.Context()
		role, _ = ctx.Value(middleware.ContextKeyRole).(rbac.Role)
	)

	if !role.Allowed("roles/resources", "list") {
		user, _ := ctx.Value(middleware.ContextKeyUser).(string)
		plog.Warn(plog.TypeSecurity, "listing role resources not allowed", "user", user)
		http.Error(w, "forbidden to list role resources", http.StatusForbidden)

		return
	}

	grouped := make(map[string][]string)
	for _, permission := range rbac.Permissions {
		grouped[permission.Resource] = append(grouped[permission.Resource], permission.Verb)
	}

	resources := make([]KnownPermission, 0, len(grouped))
	for resource, verbs := range grouped {
		sort.Strings(verbs)
		resources = append(resources, KnownPermission{Resource: resource, Verbs: verbs})
	}

	sort.Slice(resources, func(i, j int) bool {
		return resources[i].Resource < resources[j].Resource
	})

	body, err := json.Marshal(util.WithRoot("resources", resources))
	if err != nil {
		plog.Error(plog.TypeSystem, "marshaling role resources", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	_, _ = w.Write(body)
}

func CreateRole(w http.ResponseWriter, r *http.Request) {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "CreateRole")

	var (
		ctx     = r.Context()
		role, _ = ctx.Value(middleware.ContextKeyRole).(rbac.Role)
	)

	if !role.Allowed("roles", "create") {
		user, _ := ctx.Value(middleware.ContextKeyUser).(string)
		plog.Warn(plog.TypeSecurity, "creating roles not allowed", "user", user)
		http.Error(w, "forbidden to create roles", http.StatusForbidden)

		return
	}

	req, err := readRoleRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	c, err := configFromRoleRequest(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	if err = ensureRoleNameUnique(c.Metadata.Name, req.Name); err != nil {
		handleRoleConfigError(w, err, "unable to validate role name")

		return
	}

	if _, err = apiConfig.Create(apiConfig.CreateFromConfig(c), apiConfig.CreateWithValidation()); err != nil {
		handleRoleConfigError(w, err, "unable to create role")

		return
	}

	w.Header().Set("Location", strings.ToLower(fmt.Sprintf("/api/v1/roles/%s", c.Metadata.Name)))
	w.WriteHeader(http.StatusCreated)
}

func UpdateRole(w http.ResponseWriter, r *http.Request) {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "UpdateRole")

	var (
		ctx          = r.Context()
		role, _      = ctx.Value(middleware.ContextKeyRole).(rbac.Role)
		metadataName = mux.Vars(r)["name"]
	)

	if !role.Allowed("roles", "update", metadataName) {
		user, _ := ctx.Value(middleware.ContextKeyUser).(string)
		plog.Warn(plog.TypeSecurity, "updating roles not allowed", "user", user, "role", metadataName)
		http.Error(w, "forbidden to update role", http.StatusForbidden)

		return
	}

	req, err := readRoleRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	if req.MetadataName == "" {
		req.MetadataName = metadataName
	}
	if req.MetadataName != metadataName {
		http.Error(w, "role metadata name cannot be changed", http.StatusBadRequest)

		return
	}

	c, err := configFromRoleRequest(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	if err = ensureRoleNameUnique(metadataName, req.Name); err != nil {
		handleRoleConfigError(w, err, "unable to validate role name")

		return
	}

	if err = apiConfig.Update("role/"+metadataName, c); err != nil {
		handleRoleConfigError(w, err, "unable to update role")

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func DeleteRole(w http.ResponseWriter, r *http.Request) {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "DeleteRole")

	var (
		ctx          = r.Context()
		role, _      = ctx.Value(middleware.ContextKeyRole).(rbac.Role)
		metadataName = mux.Vars(r)["name"]
	)

	if !role.Allowed("roles", "delete", metadataName) {
		user, _ := ctx.Value(middleware.ContextKeyUser).(string)
		plog.Warn(plog.TypeSecurity, "deleting roles not allowed", "user", user, "role", metadataName)
		http.Error(w, "forbidden to delete role", http.StatusForbidden)

		return
	}

	if err := apiConfig.Delete("role/" + metadataName); err != nil {
		handleRoleConfigError(w, err, "unable to delete role")

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func readRoleRequest(r *http.Request) (RoleRequest, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return RoleRequest{}, fmt.Errorf("reading request body: %w", err)
	}

	var req RoleRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return RoleRequest{}, fmt.Errorf("unmarshaling request body: %w", err)
	}

	return req, nil
}

func configFromRoleRequest(req RoleRequest) (*store.Config, error) {
	if req.MetadataName == "" {
		req.MetadataName = metadataNameForRole(req.Name)
	}

	spec, err := roleSpecFromRequest(req)
	if err != nil {
		return nil, err
	}

	if req.MetadataName == "" {
		return nil, errors.New("role metadata name is required")
	}

	if !apiConfig.NameRegex.MatchString(req.MetadataName) {
		return nil, errors.New("role metadata name is not a valid format")
	}

	return &store.Config{
		Version: "phenix.sandia.gov/v1",
		Kind:    "Role",
		Metadata: store.ConfigMetadata{ //nolint:exhaustruct // partial initialization
			Name: req.MetadataName,
		},
		Spec: structs.MapDefaultCase(spec, structs.CASESNAKE),
	}, nil
}

func roleSpecFromRequest(req RoleRequest) (*v1.RoleSpec, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errors.New("role name is required")
	}

	if len(req.Policies) == 0 {
		return nil, errors.New("at least one policy is required")
	}

	spec := &v1.RoleSpec{Name: name}

	for _, policy := range req.Policies {
		resources, err := normalizePatternList(policy.Resources, "resource")
		if err != nil {
			return nil, err
		}
		if len(resources) == 0 {
			return nil, errors.New("policy resources are required")
		}

		resourceNames, err := normalizePatternList(policy.ResourceNames, "resource name")
		if err != nil {
			return nil, err
		}

		verbs := normalizeList(policy.Verbs)
		if len(verbs) == 0 {
			return nil, errors.New("policy verbs are required")
		}

		spec.Policies = append(spec.Policies, &v1.PolicySpec{
			Resources:     resources,
			ResourceNames: resourceNames,
			Verbs:         verbs,
		})
	}

	return spec, nil
}

func normalizePatternList(values []string, label string) ([]string, error) {
	normalized := normalizeList(values)
	for _, value := range normalized {
		pattern := strings.TrimPrefix(value, "!")
		if _, err := filepath.Match(pattern, "useless"); err != nil {
			return nil, fmt.Errorf("invalid %s pattern %q: %w", label, value, err)
		}
	}

	return normalized, nil
}

func normalizeList(values []string) []string {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			normalized = append(normalized, value)
		}
	}

	return normalized
}

func metadataNameForRole(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = metadataNameCleanup.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")

	return name
}

func ensureRoleNameUnique(metadataName, roleName string) error {
	roles, err := rbac.GetRoles()
	if err != nil {
		return fmt.Errorf("getting roles: %w", err)
	}

	for _, role := range roles {
		if role.MetadataName() != metadataName && role.Spec.Name == roleName {
			return fmt.Errorf("%w: %s", errRoleNameExists, roleName)
		}
	}

	return nil
}

func handleRoleConfigError(w http.ResponseWriter, err error, message string) {
	if errors.Is(err, types.ErrValidationFailed) {
		cause := errors.Unwrap(err)
		http.Error(w, cause.Error(), http.StatusBadRequest)

		return
	}

	if errors.Is(err, store.ErrExist) {
		http.Error(w, "role with same metadata name already exists", http.StatusConflict)

		return
	}

	if errors.Is(err, store.ErrNotExist) {
		http.Error(w, "role does not exist", http.StatusNotFound)

		return
	}

	if errors.Is(err, errRoleNameExists) {
		http.Error(w, err.Error(), http.StatusConflict)

		return
	}

	plog.Error(plog.TypeSystem, message, "err", err)
	http.Error(w, message, http.StatusInternalServerError)
}
