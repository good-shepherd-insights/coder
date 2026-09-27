package agentfiles_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3/sloggers/slogtest"
	"github.com/coder/coder/v2/agent/agentchat"
	"github.com/coder/coder/v2/agent/agentfiles"
	"github.com/coder/coder/v2/agent/agentgit"
	"github.com/coder/coder/v2/agent/agenttoolcall"
	"github.com/coder/coder/v2/codersdk/workspacesdk"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/quartz"
)

// TestFileToolCallRunsOnce verifies that an edit or write with tool call
// headers changes the file once: a repeated request and a cancel return
// the recorded response without touching the file again. Without tool
// call headers every request applies.
func TestFileToolCallRunsOnce(t *testing.T) {
	t.Parallel()

	filePath := filepath.Join(os.TempDir(), "work", "file.txt")
	editBody, err := json.Marshal(workspacesdk.FileEditRequest{
		Files: []workspacesdk.FileEdits{{
			Path:  filePath,
			Edits: []workspacesdk.FileEdit{{OldText: "one", NewText: "two"}},
		}},
		IncludeDiff: true,
	})
	require.NoError(t, err)

	routes := []struct {
		name   string
		target string
		body   []byte
	}{
		{name: "EditFiles", target: "/edit-files", body: editBody},
		{name: "WriteFile", target: "/write-file?path=" + url.QueryEscape(filePath), body: []byte("two\n")},
	}
	for _, route := range routes {
		for _, toolCall := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/ToolCall=%t", route.name, toolCall), func(t *testing.T) {
				t.Parallel()

				handler, fs := newFileToolCallAPI(t)
				chatID := uuid.New()
				headers := http.Header{workspacesdk.CoderChatIDHeader: {chatID.String()}}
				if toolCall {
					workspacesdk.ToolCall{MessageID: 1, ID: "call"}.SetHeaders(headers)
				}
				require.NoError(t, afero.WriteFile(fs, filePath, []byte("one\n"), 0o644))

				first := serveFileRequest(t, handler, route.target, route.body, headers)
				require.Equal(t, http.StatusOK, first.Code, first.Body.String())
				requireFileContent(t, fs, filePath, "two\n")

				// Undo the change so a second run would show on disk.
				require.NoError(t, afero.WriteFile(fs, filePath, []byte("one\n"), 0o644))
				again := serveFileRequest(t, handler, route.target, route.body, headers)
				require.Equal(t, http.StatusOK, again.Code, again.Body.String())
				assert.Equal(t, first.Body.String(), again.Body.String())
				if !toolCall {
					requireFileContent(t, fs, filePath, "two\n")
					assert.Empty(t, again.Header().Get(workspacesdk.CoderToolCallRunAgeMsHeader))
					return
				}
				requireFileContent(t, fs, filePath, "one\n")
				assert.Equal(t, "0", again.Header().Get(workspacesdk.CoderToolCallRunAgeMsHeader))

				id := workspacesdk.ToolCallUUID(chatID, 1, "call").String()
				canceled := serveFileRequest(t, handler, "/tool-calls/"+id+"/cancel", []byte(`{"stop_if_run_age_below_ms":0}`), headers)
				require.Equal(t, http.StatusOK, canceled.Code, canceled.Body.String())
				var resp workspacesdk.CancelToolCallResponse
				require.NoError(t, json.NewDecoder(canceled.Body).Decode(&resp))
				assert.True(t, resp.Started)
				assert.Equal(t, http.StatusOK, resp.StatusCode)
				assert.Equal(t, first.Header().Get("Content-Type"), resp.ContentType)
				assert.Equal(t, first.Body.Bytes(), resp.Body)
				assert.Nil(t, resp.Process)
				requireFileContent(t, fs, filePath, "one\n")
			})
		}
	}
}

// newFileToolCallAPI returns a handler that serves the file routes and
// the cancel route the way the agent mounts them, for an agent that has
// been running long enough to prove any tool call new to it.
func newFileToolCallAPI(t *testing.T) (http.Handler, afero.Fs) {
	t.Helper()

	clock := quartz.NewMock(t)
	store := agenttoolcall.NewStore(clock)
	clock.Advance(time.Hour).MustWait(testutil.Context(t, testutil.WaitShort))
	fs := afero.NewMemMapFs()
	api := agentfiles.NewAPI(slogtest.Make(t, nil), fs, agentgit.NewPathStore(), agentfiles.WithToolCallStore(store))

	router := chi.NewRouter()
	router.Post("/tool-calls/{id}/cancel", store.CancelHandler(nil))
	router.Mount("/", api.Routes())
	return agentchat.Middleware(router), fs
}

func serveFileRequest(t *testing.T, handler http.Handler, target string, body []byte, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(testutil.Context(t, testutil.WaitShort), http.MethodPost, target, bytes.NewReader(body))
	for k, v := range headers {
		r.Header[k] = v
	}
	handler.ServeHTTP(w, r)
	return w
}

func requireFileContent(t *testing.T, fs afero.Fs, path, want string) {
	t.Helper()

	got, err := afero.ReadFile(fs, path)
	require.NoError(t, err)
	require.Equal(t, want, string(got))
}
