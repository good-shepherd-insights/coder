package workspacesdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/quartz"
)

func TestReadToolCallError(t *testing.T) {
	t.Parallel()

	const jsonType = "application/json"
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
		// wantCode is the expected ToolCallError code. Empty means the
		// result must equal codersdk.ReadBodyAsError for the same response.
		wantCode ToolCallErrorCode
	}{
		{name: "stale", status: http.StatusConflict, contentType: jsonType, body: `{"code":"stale_tool_call","message":"Stale.","detail":"latest is 43"}`, wantCode: ToolCallErrorStale},
		{name: "agent started after tool call", status: http.StatusConflict, contentType: jsonType, body: `{"code":"agent_started_after_tool_call","message":"Restarted."}`, wantCode: ToolCallErrorAgentStartedAfterToolCall},
		{name: "input mismatch", status: http.StatusConflict, contentType: jsonType, body: `{"code":"input_mismatch","message":"Differs."}`, wantCode: ToolCallErrorInputMismatch},
		{name: "canceled", status: http.StatusConflict, contentType: jsonType, body: `{"code":"tool_call_canceled","message":"Canceled."}`, wantCode: ToolCallErrorCanceled},
		{name: "conflict without code", status: http.StatusConflict, contentType: jsonType, body: `{"message":"Conflict."}`},
		{name: "conflict with unknown code", status: http.StatusConflict, contentType: jsonType, body: `{"code":"start_pending","message":"Pending."}`},
		{name: "conflict without message", status: http.StatusConflict, contentType: jsonType, body: `{}`},
		{name: "conflict empty body", status: http.StatusConflict, contentType: jsonType, body: ``},
		{name: "conflict invalid JSON", status: http.StatusConflict, contentType: jsonType, body: `{"code":`},
		{name: "conflict not JSON", status: http.StatusConflict, contentType: "text/plain", body: `conflict`},
		{name: "not found with known code", status: http.StatusNotFound, contentType: jsonType, body: `{"code":"stale_tool_call","message":"Not found."}`},
		{name: "not found plain", status: http.StatusNotFound, contentType: "text/plain", body: "404 page not found\n"},
		{name: "bad request", status: http.StatusBadRequest, contentType: jsonType, body: `{"message":"Invalid tool call headers."}`},
		{name: "internal server error", status: http.StatusInternalServerError, contentType: jsonType, body: `{"message":"Boom.","detail":"spawn failed"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			response := func() *http.Response {
				rec := httptest.NewRecorder()
				rec.Header().Set("Content-Type", tc.contentType)
				rec.WriteHeader(tc.status)
				_, _ = io.WriteString(rec, tc.body)
				res := rec.Result()
				res.Request = httptest.NewRequest(http.MethodPost, "http://agent/api/v0/processes/start", nil)
				return res
			}

			res := response()
			defer res.Body.Close()
			got := readToolCallError(res)
			require.Error(t, got)

			if tc.wantCode != "" {
				var tcErr *ToolCallError
				require.ErrorAs(t, got, &tcErr)
				assert.Equal(t, tc.wantCode, tcErr.Code)
				assert.NotEmpty(t, tcErr.Message)
				return
			}

			wantRes := response()
			defer wantRes.Body.Close()
			want := codersdk.ReadBodyAsError(wantRes)
			assert.Equal(t, fmt.Sprintf("%T", want), fmt.Sprintf("%T", got))
			assert.Equal(t, want.Error(), got.Error())
			var wantSDK, gotSDK *codersdk.Error
			wantIsSDK := errors.As(want, &wantSDK)
			if assert.Equal(t, wantIsSDK, errors.As(got, &gotSDK)) && wantIsSDK {
				assert.Equal(t, wantSDK.StatusCode(), gotSDK.StatusCode())
				assert.Equal(t, wantSDK.Response, gotSDK.Response)
			}
		})
	}

	t.Run("body read failure", func(t *testing.T) {
		t.Parallel()

		readErr := xerrors.New("connection reset")
		// The partial body has a known code, but a failed read must
		// still produce the codersdk.ReadBodyAsError error.
		response := func() *http.Response {
			return &http.Response{
				StatusCode: http.StatusConflict,
				Header:     http.Header{"Content-Type": {jsonType}},
				Body:       io.NopCloser(io.MultiReader(strings.NewReader(`{"code":"stale_tool_call"`), iotest.ErrReader(readErr))),
			}
		}

		res := response()
		defer res.Body.Close()
		got := readToolCallError(res)
		require.ErrorIs(t, got, readErr)
		var tcErr *ToolCallError
		require.False(t, errors.As(got, &tcErr))

		wantRes := response()
		defer wantRes.Body.Close()
		want := codersdk.ReadBodyAsError(wantRes)
		assert.Equal(t, want.Error(), got.Error())
	})
}

func TestDoWithToolCall(t *testing.T) {
	t.Parallel()

	// The request host does not resolve, so a response proves the
	// request went over the connection dial returned.
	const unreachableURL = "http://agent.invalid:4/api/v0/processes/start"

	t.Run("age includes connect time", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		gotAge := make(chan string, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			gotAge <- r.Header.Get(CoderToolCallAgeMsHeader)
			rw.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)

		clock := quartz.NewMock(t)
		dials := 0
		dial := func(ctx context.Context) (net.Conn, error) {
			dials++
			// Waiting for a restarting agent.
			clock.Advance(5 * time.Second).MustWait(ctx)
			var d net.Dialer
			return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, unreachableURL, nil)
		require.NoError(t, err)

		res, err := doWithToolCall(req, ToolCall{MessageID: 1, ID: "call", Age: time.Second}, clock, dial)
		require.NoError(t, err)
		defer res.Body.Close()
		require.Equal(t, http.StatusOK, res.StatusCode)
		require.Equal(t, 1, dials)
		require.Equal(t, "6000", testutil.RequireReceive(ctx, t, gotAge))
	})

	t.Run("dial error", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		dialErr := xerrors.New("agent not reachable")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, unreachableURL, nil)
		require.NoError(t, err)

		res, err := doWithToolCall(req, ToolCall{MessageID: 1, ID: "call"}, quartz.NewMock(t), func(context.Context) (net.Conn, error) {
			return nil, dialErr
		})
		if res != nil {
			_ = res.Body.Close()
		}
		require.ErrorIs(t, err, dialErr)
		// Callers classify a *url.Error as a transport failure, as they
		// do for a dial inside http.Client.Do.
		var urlErr *neturl.Error
		require.ErrorAs(t, err, &urlErr)
		require.Equal(t, "Post", urlErr.Op)
		require.Empty(t, req.Header.Values(CoderToolCallAgeMsHeader))
	})

	t.Run("unused connection is closed", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		accepted := make(chan net.Conn, 1)
		go func() {
			conn, err := ln.Accept()
			if assert.NoError(t, err) {
				accepted <- conn
			}
		}()

		reqCtx, cancel := context.WithCancel(ctx)
		dial := func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			conn, err := d.DialContext(ctx, "tcp", ln.Addr().String())
			// The request ends after the connection is established.
			cancel()
			return conn, err
		}
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, unreachableURL, nil)
		require.NoError(t, err)

		res, err := doWithToolCall(req, ToolCall{MessageID: 1, ID: "call"}, quartz.NewMock(t), dial)
		if res != nil {
			_ = res.Body.Close()
		}
		require.ErrorIs(t, err, context.Canceled)
		// The transport may have written part of the request before it
		// saw the cancel; either way the connection ends.
		server := testutil.RequireReceive(ctx, t, accepted)
		defer server.Close()
		require.NoError(t, server.SetReadDeadline(time.Now().Add(testutil.WaitShort)))
		_, err = io.Copy(io.Discard, server)
		require.NoError(t, err)
	})
}

func TestRunAgeFromHeader(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		values []string
		want   time.Duration
	}{
		{name: "absent", want: 0},
		{name: "zero", values: []string{"0"}, want: 0},
		{name: "milliseconds", values: []string{"2500"}, want: 2500 * time.Millisecond},
		{name: "duration limit", values: []string{"9223372036854"}, want: 9223372036854 * time.Millisecond},
		{name: "above duration limit", values: []string{"9223372036855"}, want: 0},
		{name: "negative", values: []string{"-1"}, want: 0},
		{name: "not a number", values: []string{"abc"}, want: 0},
		{name: "empty", values: []string{""}, want: 0},
		{name: "multiple values", values: []string{"1", "2"}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := http.Header{}
			for _, v := range tc.values {
				h.Add(CoderToolCallRunAgeMsHeader, v)
			}
			require.Equal(t, tc.want, runAgeFromHeader(h))
		})
	}
}

func TestAgentAPIPath(t *testing.T) {
	t.Parallel()

	t.Run("encodes reserved query characters", func(t *testing.T) {
		t.Parallel()

		path := "/tmp/a&b ?#%c.md"
		got := agentAPIPath("/api/v0/resolve-path", neturl.Values{
			"path": []string{path},
		})

		parsed, err := neturl.Parse(got)
		require.NoError(t, err)
		require.Equal(t, "/api/v0/resolve-path", parsed.Path)
		require.Equal(t, path, parsed.Query().Get("path"))
	})

	t.Run("preserves all query values", func(t *testing.T) {
		t.Parallel()

		got := agentAPIPath("/api/v0/read-file-lines", neturl.Values{
			"path":               []string{"/tmp/plan v1#.md"},
			"offset":             []string{"10"},
			"limit":              []string{"20"},
			"max_file_size":      []string{"30"},
			"max_line_bytes":     []string{"40"},
			"max_response_lines": []string{"50"},
			"max_response_bytes": []string{"60"},
		})

		parsed, err := neturl.Parse(got)
		require.NoError(t, err)
		require.Equal(t, "/api/v0/read-file-lines", parsed.Path)
		require.Equal(t, "/tmp/plan v1#.md", parsed.Query().Get("path"))
		require.Equal(t, "10", parsed.Query().Get("offset"))
		require.Equal(t, "20", parsed.Query().Get("limit"))
		require.Equal(t, "30", parsed.Query().Get("max_file_size"))
		require.Equal(t, "40", parsed.Query().Get("max_line_bytes"))
		require.Equal(t, "50", parsed.Query().Get("max_response_lines"))
		require.Equal(t, "60", parsed.Query().Get("max_response_bytes"))
	})

	t.Run("debug logs zero after", func(t *testing.T) {
		t.Parallel()

		got := debugLogsPath(time.Time{})
		require.Equal(t, "/debug/logs", got)
	})

	t.Run("debug logs after", func(t *testing.T) {
		t.Parallel()

		after := time.Date(2026, 5, 18, 12, 34, 56, 789, time.FixedZone("test", -7*60*60))
		got := debugLogsPath(after)
		parsed, err := neturl.Parse(got)
		require.NoError(t, err)
		require.Equal(t, "/debug/logs", parsed.Path)
		require.Equal(t, after.UTC().Format(time.RFC3339Nano), parsed.Query().Get("after"))
	})
}
