package bulk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"reflect"

	"github.com/spf13/afero"
	"github.com/tarunKoyalwar/restish/cli"
	"github.com/zeebo/xxh3"
)

// reformat returns the standardized/formatted JSON representation given JSON
// byte data as input.
func reformat(data []byte) ([]byte, error) {
	// Round-trip to get consistent formatting. This is inefficient but a much
	// nicer experience for people with auto-formatters set up in their editor
	// or who may try to undo changes and get the formatting slightly off.
	var tmp any
	json.Unmarshal(data, &tmp)
	return cli.MarshalShort("json", true, tmp)
}

// hash returns a new fast 128-bit hash of the given bytes.
func hash(b []byte) []byte {
	tmp := xxh3.Hash128(b).Bytes()
	return tmp[:]
}

// File represents a checked out file with metadata about the remote and local
// version(s) of the file.
type File struct {
	// Path is the relative path to the local file
	Path string `json:"path"`
	// URL to the remote file
	URL string `json:"url"`

	// ETag header used for conditional updates
	ETag string `json:"etag,omitempty"`
	// LastModified header used for conditional updates
	LastModified string `json:"last_modified,omitempty"`

	// VersionRemote used to compare when listing
	VersionRemote string `json:"version_remote,omitempty"`
	// VersionLocal tracks the local copy of the file
	VersionLocal string `json:"version_local,omitempty"`

	// Schema is used to describe the type of the resource, if available.
	Schema string `json:"schema,omitempty"`

	// Hash is used for detecting local changes
	Hash []byte `json:"hash,omitempty"`
}

// GetData returns the file contents.
func (f *File) GetData() ([]byte, error) {
	return afero.ReadFile(afs, f.Path)
}

// IsChangedLocal returns whether a file has been modified locally. The
// `ignoreDeleted` parameter sets whether deleted files are considered to be
// changed or not.
func (f *File) IsChangedLocal(ignoreDeleted bool) bool {
	if len(f.Hash) == 0 {
		return false
	}
	b, err := f.GetData()
	if err != nil {
		return !ignoreDeleted
	}

	b, err = reformat(b)
	if err != nil {
		cli.LogWarning("Warning unable to format %s: %s\n", f.Path, err)
		return false
	}

	return !bytes.Equal(hash(b), f.Hash)
}

// IsChangedRemote returns whether the local and remote versions mismatch.
func (f *File) IsChangedRemote() bool {
	return f.VersionLocal != f.VersionRemote
}

// Fetch pulls the remote file and updates the metadata.
func (f *File) Fetch() ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, f.URL, nil)
	// TODO: conditional fetch?
	resp, err := cli.GetParsedResponse(req)
	if err != nil {
		return nil, err
	}

	if resp.Status >= http.StatusBadRequest {
		cli.LogError("Error fetching %s from %s\n", f.Path, f.URL)
		cli.Formatter.Format(resp)
		return nil, fmt.Errorf("error fetching %s", f.URL)
	}

	if etag := resp.Headers["Etag"]; etag != "" {
		f.ETag = etag
	}

	if lastModified := resp.Headers["Last-Modified"]; lastModified != "" {
		f.LastModified = lastModified
	}

	if db := resp.Links["describedby"]; len(db) > 0 {
		baseURL, _ := url.Parse(f.URL)
		u, _ := url.Parse(db[0].URI)
		f.Schema = baseURL.ResolveReference(u).String()
	} else {
		v := reflect.ValueOf(resp.Body)
		if v.Kind() == reflect.Map && !v.IsNil() {
			if s := v.MapIndex(reflect.ValueOf("$schema")); s.Kind() == reflect.String {
				// Assume this is not a relative URL as it lives within the doc.
				f.Schema = v.String()
			}
		}
	}

	b, err := cli.MarshalShort("json", true, resp.Body)
	if err != nil {
		return nil, err
	}

	f.VersionLocal = f.VersionRemote

	if err := f.WriteCached(b); err != nil {
		return nil, err
	}

	return b, nil
}

// WriteCached writes the file to disk in the special cache directory.
func (f *File) WriteCached(b []byte) error {
	fp := path.Join(".rshbulk", f.Path)
	afs.MkdirAll(filepath.Dir(fp), 0700)
	return afero.WriteFile(afs, fp, b, 0600)
}

// Write writes the file to disk. This also updates the local file hash
// used to determine if the file has been modified.
func (f *File) Write(b []byte) error {
	f.Hash = hash(b)
	afs.MkdirAll(filepath.Dir(f.Path), 0700)
	return afero.WriteFile(afs, f.Path, b, 0600)
}

// Reset overwrites the local file with the remote contents.
func (f *File) Reset() error {
	cached, err := afero.ReadFile(afs, path.Join(metaDir, f.Path))
	if err != nil {
		return err
	}
	return f.Write(cached)
}
