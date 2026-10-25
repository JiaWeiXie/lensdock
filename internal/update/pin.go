package update

import (
	"context"
	"fmt"
)

// CheckImageRef reports whether ref is a repository[:tag][@sha256:digest]
// reference that is safe to use as an argv element and in a compose env file.
func CheckImageRef(ref string) error { return checkImageRef(ref) }

// CheckComposeProject reports whether name is a valid compose project name
// for deployment.json.
func CheckComposeProject(name string) error {
	if !projectPattern.MatchString(name) {
		return fmt.Errorf("compose project %q must match %s", name, projectPattern)
	}
	return nil
}

// PinImage resolves a configured image reference that exists locally to
// repository@digest and verifies that Docker can address the image by it.
// A reference that already carries a digest is verified, not changed.
func (e *Engine) PinImage(ctx context.Context, configured string) (ref, digest string, err error) {
	return e.pinImage(ctx, configured, "")
}
