package hostmanager

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// canonicalDir resolves an existing directory to its symlink-free absolute
// path and filesystem identity.
func canonicalDir(path string) (string, DirectoryIdentity, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return "", DirectoryIdentity{}, errors.New("a project path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", DirectoryIdentity{}, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", DirectoryIdentity{}, fmt.Errorf("project path: %w", err)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", DirectoryIdentity{}, fmt.Errorf("project path: %w", err)
	}
	if !fi.IsDir() {
		return "", DirectoryIdentity{}, fmt.Errorf("project path %s is not a directory", real)
	}
	id, ok := identityOf(fi)
	if !ok {
		return "", DirectoryIdentity{}, errors.New("this platform does not report directory identity")
	}
	return real, id, nil
}

// resolveLoose canonicalizes the deepest existing ancestor of path and appends
// the missing components, so a path below a deleted project directory still
// maps to its registered root.
func resolveLoose(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	cur := abs
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(real, rest), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// within reports whether child equals parent or lies below it, component-wise.
func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func overlaps(a, b string) bool { return within(a, b) || within(b, a) }

// checkDirectory verifies that the registered directory still exists with the
// authorized identity. It never repairs or retargets a grant.
func checkDirectory(p *projectRecord) error {
	fi, err := os.Stat(p.Root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("authorized directory %s is missing; restore it or relocate the project", p.Root)
		}
		return fmt.Errorf("authorized directory %s: %w", p.Root, err)
	}
	id, ok := identityOf(fi)
	if !fi.IsDir() || !ok || id != p.Directory {
		return fmt.Errorf("authorized directory %s was replaced; relocate or revoke the project", p.Root)
	}
	return nil
}

// projectForPath finds the registered project containing the resolved path.
// An active grant wins over a revoked one.
func (r *registry) projectForPath(resolved string) *projectRecord {
	var revoked *projectRecord
	for _, p := range r.Projects {
		if !within(p.Root, resolved) {
			continue
		}
		if !p.Revoked {
			return p
		}
		revoked = p
	}
	return revoked
}

// ResolveProject maps a path inside an authorized project to its registered
// project. It reads public registry metadata only: no socket, no Docker and no
// credential. Callers then use Project.TokenFile for project-scoped requests.
func ResolveProject(root, path string) (Project, error) {
	canonical, err := canonicalRoot(root)
	if err != nil {
		return Project{}, err
	}
	if err := checkPrivateDir(canonical); err != nil {
		return Project{}, err
	}
	if _, err := LoadConfig(canonical); err != nil {
		return Project{}, err
	}
	reg, err := loadRegistry(canonical)
	if err != nil {
		return Project{}, err
	}
	resolved, err := resolveLoose(path)
	if err != nil {
		return Project{}, fmt.Errorf("project path: %w", err)
	}
	rec := reg.projectForPath(resolved)
	if rec == nil {
		return Project{}, fmt.Errorf("%s is not inside an authorized project; ask the human to run `lensdock admin project authorize`", resolved)
	}
	if rec.Revoked {
		return Project{}, fmt.Errorf("the grant for %s was revoked", rec.Root)
	}
	if err := checkDirectory(rec); err != nil {
		return Project{}, err
	}
	return buildProject(canonical, reg, rec, nil), nil
}
