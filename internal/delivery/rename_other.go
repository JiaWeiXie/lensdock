//go:build !linux && !darwin

package delivery

// renameNoReplace moves the directory from to the absent path to.
func renameNoReplace(from, to string) error { return renameChecked(from, to) }
