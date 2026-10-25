package feedback

import (
	"archive/zip"
	"bytes"
	"context"
	"strconv"
)

// BundleData is a zip of one published revision.
type BundleData struct {
	Name string
	Zip  []byte
}

// ReadBundle returns the share bundle of an authorized revision as a zip with
// fixed, flat file names (feedback.json, task.md, annotations.json,
// environment.json, both screenshots, manifest.json). Every file is verified
// against its recorded hash first. Raw images are not part of any bundle.
func (s *Service) ReadBundle(ctx context.Context, fbID string, rev int) (*BundleData, error) {
	_, r, _, err := s.authRead(ctx, "bundle.read", fbID, rev)
	if err != nil {
		return nil, err
	}
	names := append(append([]string{}, bundleFileNames...), ArtifactManifest)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mod := fromMS(r.PublishedAt.Int64)
	for _, name := range names {
		a, err := s.readArtifact(ctx, r, name)
		if err != nil {
			return nil, err
		}
		method := zip.Deflate
		if a.MimeType == mimePNG {
			method = zip.Store
		}
		hdr := &zip.FileHeader{Name: name, Method: method, Modified: mod}
		hdr.SetMode(0o644)
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return nil, s.internal("bundle.read", err)
		}
		if _, err := fw.Write(a.Data); err != nil {
			return nil, s.internal("bundle.read", err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, s.internal("bundle.read", err)
	}
	return &BundleData{Name: "lensdock-" + r.FeedbackID + "-r" + strconv.Itoa(r.Revision) + ".zip", Zip: buf.Bytes()}, nil
}
