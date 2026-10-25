package update

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// MergeDevManifests combines single-architecture dev manifests produced from
// the same source into one multi-architecture dev manifest. Evidence files are
// copied and rehashed; mismatched release/source/base/component/schema facts
// are rejected.
func MergeDevManifests(outputDir string, inputDirs []string) (*Manifest, error) {
	if len(inputDirs) < 2 {
		return nil, errors.New("merge needs at least two dev release directories")
	}
	if err := os.Mkdir(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create merged release directory: %w", err)
	}
	removeOnError := true
	defer func() {
		if removeOnError {
			_ = os.RemoveAll(outputDir)
		}
	}()
	var merged *Manifest
	seenArch := map[string]bool{}
	sources := map[string]bool{}
	for _, dir := range inputDirs {
		raw, err := readFileLimit(filepath.Join(dir, ManifestFileName), maxManifestBytes)
		if err != nil {
			return nil, err
		}
		m, err := DecodeManifest(raw)
		if err != nil {
			return nil, err
		}
		if err := m.Validate(); err != nil {
			return nil, err
		}
		if m.Channel != ChannelDev || len(m.Images) != 1 || len(m.CLI) != 1 {
			return nil, fmt.Errorf("%s is not a single-architecture dev manifest", dir)
		}
		if merged == nil {
			base := m
			base.Images, base.CLI, base.APT.Packages, base.APT.Sources = map[string]ArchImages{}, map[string]CLIBinary{}, nil, nil
			merged = &base
		} else if m.ReleaseID != merged.ReleaseID || m.Source != merged.Source || m.Base != merged.Base || m.Components != merged.Components || m.Schema != merged.Schema || m.Rollback != merged.Rollback {
			return nil, fmt.Errorf("%s has release/source/base/component/schema facts different from the first manifest", dir)
		}
		if m.CreatedAt.After(merged.CreatedAt) {
			merged.CreatedAt = m.CreatedAt
		}
		if m.APT.ObservedAt.After(merged.APT.ObservedAt) {
			merged.APT.ObservedAt = m.APT.ObservedAt
		}
		for _, src := range m.APT.Sources {
			sources[src] = true
		}
		merged.APT.Packages = append(merged.APT.Packages, m.APT.Packages...)
		for arch, imgs := range m.Images {
			if seenArch[arch] {
				return nil, fmt.Errorf("architecture %s appears more than once", arch)
			}
			seenArch[arch] = true
			var err error
			imgs.Runtime, err = mergeImageEvidence(outputDir, dir, "runtime-"+arch, imgs.Runtime)
			if err != nil {
				return nil, err
			}
			if imgs.Gateway != nil {
				gw, err := mergeImageEvidence(outputDir, dir, "gateway-"+arch, *imgs.Gateway)
				if err != nil {
					return nil, err
				}
				imgs.Gateway = &gw
			}
			merged.Images[arch] = imgs
			merged.CLI[arch] = m.CLI[arch]
		}
	}
	for src := range sources {
		merged.APT.Sources = append(merged.APT.Sources, src)
	}
	sort.Strings(merged.APT.Sources)
	sort.Slice(merged.APT.Packages, func(i, j int) bool {
		if merged.APT.Packages[i].Arch != merged.APT.Packages[j].Arch {
			return merged.APT.Packages[i].Arch < merged.APT.Packages[j].Arch
		}
		return merged.APT.Packages[i].Name < merged.APT.Packages[j].Name
	})
	if err := merged.Validate(); err != nil {
		return nil, err
	}
	if err := writeJSONAtomic(filepath.Join(outputDir, ManifestFileName), merged, 0o644); err != nil {
		return nil, err
	}
	removeOnError = false
	return merged, nil
}

func mergeImageEvidence(outputDir, inputDir, name string, ref ImageRef) (ImageRef, error) {
	if ref.SBOM != nil {
		rel := "evidence/" + name + ".local.sbom.json"
		att, err := copyEvidence(outputDir, rel, filepath.Join(inputDir, filepath.FromSlash(ref.SBOM.Path)))
		if err != nil {
			return ref, err
		}
		att.SubjectDigest = ref.SBOM.SubjectDigest
		ref.SBOM.Attachment = att
	}
	return ref, nil
}
