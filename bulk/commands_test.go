package bulk

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
	"github.com/tarunKoyalwar/restish/cli"
	"gopkg.in/h2non/gock.v1"
)

func run(cmd ...string) (string, error) {
	capture := &strings.Builder{}
	cli.Stdout = capture
	cli.Stderr = capture
	cli.Root.SetOut(capture)
	os.Args = append([]string{"restish"}, cmd...)
	err := cli.Run()

	return capture.String(), err
}

func mustExist(t *testing.T, path string) {
	_, err := afs.Stat(path)
	if err != nil {
		paths := []string{}
		afero.Walk(afs, "", func(path string, info fs.FileInfo, err error) error {
			paths = append(paths, path)
			return nil
		})
		t.Fatalf("Expected path %s not found in fs: %v", path, paths)
	}
}

func mustEqualJSON(t *testing.T, path string, contents string) {
	mustExist(t, path)
	b, err := afero.ReadFile(afs, path)
	require.NoError(t, err)
	require.JSONEq(t, string(b), contents)
}

func mustContain(t *testing.T, path string, contents string) {
	mustExist(t, path)
	b, err := afero.ReadFile(afs, path)
	require.NoError(t, err)
	require.Contains(t, string(b), contents)
}

func mustHaveCalledAllHTTPMocks(t *testing.T) {
	if !gock.IsDone() {
		requests := []string{}
		for _, mock := range gock.Pending() {
			requests = append(requests, mock.Request().Method+" "+mock.Request().URLStruct.String())
		}
		require.True(t, gock.IsDone(), "Not all HTTP mocks were called:\n%s", strings.Join(requests, "\n"))
	}
}

type remoteFile struct {
	User    string `json:"user"`
	ID      string `json:"id"`
	Version string `json:"version"`
	body    string
	fetch   bool
}

func expectRemote(files []remoteFile) {
	gock.New("https://example.com").
		Get("/all-items").
		Reply(http.StatusOK).
		JSON(files)

	for _, f := range files {
		if f.fetch {
			expectRemoteFile(f)
		}
	}
}

func expectRemoteFile(f remoteFile) {
	body := f.body
	if body == "" {
		body = fmt.Sprintf(`{"id": "%s"}`, f.ID)
	}

	gock.New("https://example.com").
		Get("/users/"+f.User+"/items/"+f.ID).
		Reply(http.StatusOK).
		SetHeader("Content-Type", "application/json").
		SetHeader("Etag", string(hash([]byte(body)))).
		SetHeader("Last-Modified", time.Now().Format(http.TimeFormat)).
		BodyString(body)
}

/*
Test workflow:
- init (pull-index, pull)
- list / list with filter
- edit some files remotely
- status
- diff remote
- pull
- edit some files locally
- reset a file
- status
- diff
- push
*/
func TestWorkflow(t *testing.T) {
	defer gock.Off()

	expectRemote([]remoteFile{
		{User: "a", ID: "a1", Version: "a11", fetch: true},
		{User: "a", ID: "a2", Version: "a21", fetch: true},
		{User: "b", ID: "b1", Version: "b11", fetch: true},
		{User: "c", ID: "c1", Version: "c11", fetch: true},
	})

	afs = afero.NewMemMapFs()

	cli.Init("test", "1.0.0")
	cli.Defaults()
	Init(cli.Root)

	// Init
	// ====
	run("bulk", "init", "example.com/all-items", "--url-template=/users/{user}/items/{id}")

	mustExist(t, ".rshbulk")
	mustExist(t, ".rshbulk/meta")
	mustEqualJSON(t, "a/items/a1.json", `{"id": "a1"}`)
	mustEqualJSON(t, "a/items/a2.json", `{"id": "a2"}`)
	mustEqualJSON(t, "b/items/b1.json", `{"id": "b1"}`)
	mustEqualJSON(t, "c/items/c1.json", `{"id": "c1"}`)
	mustHaveCalledAllHTTPMocks(t)

	// List
	// ----
	gock.Flush()
	out, err := run("bulk", "list")
	require.NoError(t, err)
	require.Contains(t, out, "a/items/a1.json")
	require.Contains(t, out, "a/items/a2.json")
	require.Contains(t, out, "b/items/b1.json")
	require.Contains(t, out, "c/items/c1.json")

	// List with match query
	// ---------------------
	gock.Flush()
	out, err = run("bulk", "list", "-m", "id contains 1")
	require.NoError(t, err)
	require.Contains(t, out, "a/items/a1.json")
	require.NotContains(t, out, "a/items/a2.json")
	require.Contains(t, out, "b/items/b1.json")
	require.Contains(t, out, "c/items/c1.json")

	// List with filter to show contents
	// ---------------------------------
	gock.Flush()
	out, err = run("bulk", "list", "-m", "", "-f", "id")
	require.NoError(t, err)
	require.Contains(t, out, `"a1"`)
	require.Contains(t, out, `"a2"`)
	require.Contains(t, out, `"b1"`)
	require.Contains(t, out, `"c1"`)

	// Remote files changed
	// --------------------
	gock.Flush()

	expectRemote([]remoteFile{
		{User: "a", ID: "a1", Version: "a11"},
		{User: "a", ID: "a2", Version: "a21"},
		{User: "b", ID: "b1", Version: "b12"},
		{User: "d", ID: "d1", Version: "d11"},
	})

	metaFileContents, err := afero.ReadFile(afs, ".rshbulk/meta")
	require.NoError(t, err)

	out, err = run("bulk", "status")
	require.NoError(t, err)
	require.Contains(t, out, "Remote changes")
	require.Contains(t, out, "modified:  b/items/b1.json")
	require.Contains(t, out, "removed:  c/items/c1.json")
	require.Contains(t, out, "added:  d/items/d1.json")
	mustHaveCalledAllHTTPMocks(t)

	// The status command should never change the metadata!
	mfc2, _ := afero.ReadFile(afs, ".rshbulk/meta")
	require.Equal(t, string(metaFileContents), string(mfc2))

	// Diff remote changes
	// -------------------
	gock.Flush()

	expectRemote([]remoteFile{
		{User: "a", ID: "a1", Version: "a11"},
		{User: "a", ID: "a2", Version: "a21"},
		{User: "b", ID: "b1", Version: "b12", body: `{"id": "b1", "foo": 1}`, fetch: true},
		{User: "d", ID: "d1", Version: "d11", fetch: true},
	})

	out, err = run("bulk", "diff", "--remote")
	require.NoError(t, err)
	require.Contains(t, out, "+  \"foo\": 1")
	require.Contains(t, out, "-{\n-  \"id\": \"c1\"\n-}")
	require.Contains(t, out, "+{\n+  \"id\": \"d1\"\n+}")
	mustHaveCalledAllHTTPMocks(t)

	// Pull changes
	// ------------
	gock.Flush()

	expectRemote([]remoteFile{
		{User: "a", ID: "a1", Version: "a11"},
		{User: "a", ID: "a2", Version: "a21"},
		{User: "b", ID: "b1", Version: "b12", body: `{"id": "b1", "foo": "bar"}`, fetch: true},
		{User: "d", ID: "d1", Version: "d11", fetch: true},
	})

	_, err = run("-v", "bulk", "pull")
	require.NoError(t, err)
	mustContain(t, ".rshbulk/meta", "a21")
	mustContain(t, ".rshbulk/meta", "b12")
	mustContain(t, ".rshbulk/meta", "d11")
	mustExist(t, ".rshbulk/d/items/d1.json")
	mustEqualJSON(t, "b/items/b1.json", `{"id": "b1", "foo": "bar"}`)
	mustEqualJSON(t, "d/items/d1.json", `{"id": "d1"}`)
	_, err = afs.Stat("c/items/c1.json")
	require.Error(t, err)
	mustHaveCalledAllHTTPMocks(t)

	gock.Flush()
	expectRemote([]remoteFile{
		{User: "a", ID: "a1", Version: "a11"},
		{User: "a", ID: "a2", Version: "a21"},
		{User: "b", ID: "b1", Version: "b12", body: `{"id": "b1", "foo": "bar"}`},
		{User: "d", ID: "d1", Version: "d11"},
	})

	out, err = run("bulk", "status")
	require.NoError(t, err)
	require.Contains(t, out, "You are up to date with https://example.com")
	require.Contains(t, out, "No local changes")
	mustHaveCalledAllHTTPMocks(t)

	// Edit local files
	// ----------------
	gock.Flush()

	expectRemote([]remoteFile{
		{User: "a", ID: "a1", Version: "a11"},
		{User: "a", ID: "a2", Version: "a21"},
		{User: "b", ID: "b1", Version: "b12", body: `{"id": "b1", "foo": "bar"}`},
		{User: "d", ID: "d1", Version: "d11"},
	})

	afero.WriteFile(afs, "a/items/a1.json", []byte(`{"id": "a1", "labels": ["one"]}`), 0600)
	afs.Remove("a/items/a2.json")
	afs.Remove("d/items/d1.json")
	afero.WriteFile(afs, "a/items/a3.json", []byte(`{"id": "a3"}`), 0600)

	// Whoops, let's reset one of the files before getting the status!
	_, err = run("bulk", "reset", "a/items/a2.json")
	require.NoError(t, err)

	out, err = run("bulk", "status")
	require.NoError(t, err)
	require.Contains(t, out, "Local changes")
	require.Contains(t, out, "modified:  a/items/a1.json")
	require.Contains(t, out, "removed:  d/items/d1.json")
	require.Contains(t, out, "added:  a/items/a3.json")
	mustHaveCalledAllHTTPMocks(t)

	// Show diff
	// ---------
	gock.Flush()

	expectRemoteFile(remoteFile{User: "a", ID: "a1", Version: "a11"})

	// Ugh, flag state is saved between runs, so explicitly set to false here.
	out, err = run("bulk", "diff", "--remote=false")
	require.NoError(t, err)
	require.Contains(t, out, "--- remote https://example.com/users/a/items/a1")
	require.Contains(t, out, "+++ local a/items/a1.json")
	require.Contains(t, out, "+  \"labels\": [\n+    \"one\"\n+  ]")

	// Push changes
	// ------------
	gock.Flush()

	expectRemote([]remoteFile{
		{User: "a", ID: "a1", Version: "a11"},
		{User: "a", ID: "a2", Version: "a21"},
		{User: "b", ID: "b1", Version: "b12", body: `{"id": "b1", "foo": "bar"}`},
		{User: "d", ID: "d1", Version: "d11"},
	})

	gock.New("https://example.com").
		Put("/users/a/items/a1").
		Reply(http.StatusOK)

	gock.New("https://example.com").
		Put("/users/a/items/a3").
		Reply(http.StatusOK)

	gock.New("https://example.com").
		Delete("/users/d/items/d1").
		Reply(http.StatusNoContent)

	// Remote has changed after push!
	expectRemote([]remoteFile{
		{User: "a", ID: "a1", Version: "a12", fetch: true},
		{User: "a", ID: "a2", Version: "a21"},
		{User: "a", ID: "a3", Version: "a31", fetch: true},
		{User: "b", ID: "b1", Version: "b12", body: `{"id": "b1", "foo": "bar"}`},
	})

	out, err = run("bulk", "push")
	require.NoError(t, err)
	require.Contains(t, out, "Push complete")
	mustHaveCalledAllHTTPMocks(t)

	// Status should be empty
	// ----------------------
	gock.Flush()

	expectRemote([]remoteFile{
		{User: "a", ID: "a1", Version: "a12"},
		{User: "a", ID: "a2", Version: "a21"},
		{User: "a", ID: "a3", Version: "a31"},
		{User: "b", ID: "b1", Version: "b12", body: `{"id": "b1", "foo": "bar"}`},
	})

	out, err = run("bulk", "status")
	require.NoError(t, err)
	require.Contains(t, out, "You are up to date with https://example.com")
	require.Contains(t, out, "No local changes")
	mustHaveCalledAllHTTPMocks(t)

	// Diff should be empty
	// --------------------
	gock.Flush()

	out, err = run("bulk", "diff")
	require.NoError(t, err)
	require.Contains(t, out, "No local changes")
	mustHaveCalledAllHTTPMocks(t)
}

// TestPullFailure simulates a partial pull failure. The command should still
// complete successfully, but the failed file should be left alone. The state
// should be in a good place to retry the pull.
func TestPullFailure(t *testing.T) {
	defer gock.Off()

	expectRemote([]remoteFile{
		{User: "a", ID: "a1", Version: "a11", fetch: true},
		{User: "a", ID: "a2", Version: "a21", fetch: false},
		{User: "b", ID: "b1", Version: "b11", fetch: true},
	})

	// Simulate a pull failure on the server.
	gock.New("https://example.com").
		Get("/users/a/items/a2").
		Reply(http.StatusInternalServerError)

	afs = afero.NewMemMapFs()

	cli.Init("test", "1.0.0")
	cli.Defaults()
	Init(cli.Root)

	// Init
	// ====
	run("bulk", "init", "example.com/all-items", "--url-template=/users/{user}/items/{id}")

	mustExist(t, ".rshbulk")
	mustExist(t, ".rshbulk/meta")
	mustEqualJSON(t, "a/items/a1.json", `{"id": "a1"}`)
	_, err := afs.Stat("a/items/a2.json")
	require.Error(t, err)
	mustEqualJSON(t, "b/items/b1.json", `{"id": "b1"}`)
	mustHaveCalledAllHTTPMocks(t)

	// Status should show the one failed file as needing to be pulled.
	// ----------------------------------------------------------------
	gock.Flush()

	expectRemote([]remoteFile{
		{User: "a", ID: "a1", Version: "a11", fetch: false},
		{User: "a", ID: "a2", Version: "a21", fetch: false},
		{User: "b", ID: "b1", Version: "b11", fetch: false},
	})

	out, err := run("bulk", "status")
	require.NoError(t, err)
	require.Contains(t, out, "Remote changes")
	require.NotContains(t, out, "a/items/a1.json")
	require.Contains(t, out, "added:  a/items/a2.json")
	require.NotContains(t, out, "b/items/b1.json")
	mustHaveCalledAllHTTPMocks(t)
}

func TestPushFailure(t *testing.T) {
	defer gock.Off()

	expectRemote([]remoteFile{
		{User: "a", ID: "a1", Version: "a11", fetch: true},
		{User: "a", ID: "a2", Version: "a21", fetch: true},
		{User: "b", ID: "b1", Version: "b11", fetch: true},
	})

	afs = afero.NewMemMapFs()

	cli.Init("test", "1.0.0")
	cli.Defaults()
	Init(cli.Root)

	// Init
	// ====
	run("bulk", "init", "example.com/all-items", "--url-template=/users/{user}/items/{id}")

	mustExist(t, ".rshbulk")
	mustExist(t, ".rshbulk/meta")
	mustEqualJSON(t, "a/items/a1.json", `{"id": "a1"}`)
	mustEqualJSON(t, "a/items/a2.json", `{"id": "a2"}`)
	mustEqualJSON(t, "b/items/b1.json", `{"id": "b1"}`)
	mustHaveCalledAllHTTPMocks(t)

	// Modify files
	// ------------

	afero.WriteFile(afs, "a/items/a1.json", []byte(`{"id": "a1", "labels": ["one"]}`), 0600)
	afero.WriteFile(afs, "a/items/a2.json", []byte(`{"id": "a2", "labels": ["two"]}`), 0600)
	afero.WriteFile(afs, "b/items/b1.json", []byte(`{"id": "b1", "labels": ["three"]}`), 0600)

	// Push with partial failure
	// -------------------------
	gock.Flush()

	expectRemote([]remoteFile{
		{User: "a", ID: "a1", Version: "a11"},
		{User: "a", ID: "a2", Version: "a21"},
		{User: "b", ID: "b1", Version: "b11"},
	})

	gock.New("https://example.com").
		Put("/users/a/items/a1").
		Reply(http.StatusOK)

	gock.New("https://example.com").
		Put("/users/a/items/a2").
		Reply(http.StatusBadRequest) // <--- simulate invalid input

	gock.New("https://example.com").
		Put("/users/b/items/b1").
		Reply(http.StatusOK)

	// Remote has changed after push!
	expectRemote([]remoteFile{
		{User: "a", ID: "a1", Version: "a12", fetch: true},
		{User: "a", ID: "a2", Version: "a22", fetch: false},
		{User: "b", ID: "b1", Version: "b12", fetch: true},
	})

	out, err := run("bulk", "push")
	require.NoError(t, err)
	require.Contains(t, out, "Push complete")
	mustHaveCalledAllHTTPMocks(t)

	// Status should show the one failed file as needing to still be pushed.
	// ---------------------------------------------------------------------
	gock.Flush()

	expectRemote([]remoteFile{
		{User: "a", ID: "a1", Version: "a12"},
		{User: "a", ID: "a2", Version: "a22"},
		{User: "b", ID: "b1", Version: "b12"},
	})

	out, err = run("bulk", "status")
	require.NoError(t, err)
	require.Contains(t, out, "Local changes")
	require.NotContains(t, out, "a/items/a1.json")
	require.Contains(t, out, "modified:  a/items/a2.json")
	require.NotContains(t, out, "b/items/b1.json")
	mustHaveCalledAllHTTPMocks(t)
}

func TestFalsey(t *testing.T) {
	for _, item := range []any{false, 0, 0.0, "", []byte{}, []any{}, map[string]any{}, map[any]any{}} {
		t.Run(fmt.Sprintf("%T-%+v", item, item), func(t *testing.T) {
			require.True(t, isFalsey(item))
		})
	}
}

func TestInterpreterWithSchema(t *testing.T) {
	defer gock.Off()

	gock.New("https://example.com").
		Get("/schemas/user.json").
		Reply(http.StatusOK).
		SetHeader("Content-Type", "application/json").
		BodyString(`{
			"type": "object",
			"properties": {
				"name": {
					"type": "string"
				},
				"trinkets": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {
							"age": {
								"type": "number"
							}
						}
					}
				}
			}
		}`)

	capture := &strings.Builder{}
	cli.Stdout = capture
	cli.Stderr = capture
	newInterpreter("trinkets where age > 5", "https://example.com/schemas/user.json")
	require.NotContains(t, capture.String(), "WARN")
}

func TestInterpreterWithSchemaWarning(t *testing.T) {
	defer gock.Off()

	gock.New("https://example.com").
		Get("/schemas/user.json").
		Reply(http.StatusOK).
		SetHeader("Content-Type", "application/json").
		BodyString(`{
			"type": "object",
			"properties": {
				"name": {
					"type": "string"
				},
				"trinkets": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {
							"age": {
								"type": "number"
							}
						}
					}
				}
			}
		}`)

	capture := &strings.Builder{}
	cli.Stdout = capture
	cli.Stderr = capture
	newInterpreter("name > 5", "https://example.com/schemas/user.json")
	require.Contains(t, capture.String(), "WARN: cannot compare string with number")
}

func TestInterpreterWithSchema404(t *testing.T) {
	defer gock.Off()

	gock.New("https://example.com").
		Get("/schemas/user.json").
		Reply(http.StatusNotFound)

	capture := &strings.Builder{}
	cli.Stdout = capture
	cli.Stderr = capture
	newInterpreter("name contains foo", "https://example.com/schemas/user.json")
	require.NotContains(t, capture.String(), "WARN")
}

func TestInterpreterWithSchemaInvalid(t *testing.T) {
	defer gock.Off()

	gock.New("https://example.com").
		Get("/schemas/user.json").
		Reply(http.StatusOK).
		SetHeader("Content-Type", "application/json").
		BodyString(`{bad json}`)

	capture := &strings.Builder{}
	cli.Stdout = capture
	cli.Stderr = capture
	newInterpreter("name contains foo", "https://example.com/schemas/user.json")
	require.NotContains(t, capture.String(), "WARN")
}
