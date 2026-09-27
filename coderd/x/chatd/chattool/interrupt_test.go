package chattool_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/coder/coder/v2/coderd/x/chatd/chattool"
	"github.com/coder/coder/v2/codersdk/workspacesdk"
	"github.com/coder/coder/v2/codersdk/workspacesdk/agentconnmock"
	"github.com/coder/coder/v2/testutil"
)

// TestInterruptedCall covers which unresolved calls interrupt handling
// cancels, the stop run age each cancel request carries, and the tool
// response each tool's answer becomes.
func TestInterruptedCall(t *testing.T) {
	t.Parallel()

	const notRunContent = `{"success":false,"exit_code":0,"wall_duration_ms":0,"error":"not run: the command was canceled before the workspace agent received it."}`

	tests := []struct {
		name     string
		toolName string
		input    string
		// wantCancel is false when the call keeps the generic interrupted
		// result without a cancel request.
		wantCancel bool
		wantStop   time.Duration
		// resp is the agent's answer, and wantContent the text of the
		// tool response it becomes.
		resp        workspacesdk.CancelToolCallResponse
		wantContent string
	}{
		{
			name:        "ExecuteForeground",
			toolName:    chattool.ExecuteToolName,
			input:       `{"command":"make test","timeout":"2h"}`,
			wantCancel:  true,
			wantStop:    2 * time.Hour,
			wantContent: notRunContent,
		},
		{
			name:        "ExecuteDefaultTimeout",
			toolName:    chattool.ExecuteToolName,
			input:       `{"command":"make test"}`,
			wantCancel:  true,
			wantStop:    chattool.ExecuteDefaultTimeout,
			wantContent: notRunContent,
		},
		{
			// A background process never stops.
			name:        "ExecuteBackground",
			toolName:    chattool.ExecuteToolName,
			input:       `{"command":"make dev","run_in_background":true}`,
			wantCancel:  true,
			wantContent: notRunContent,
		},
		{
			name:     "ExecuteInvalidTimeout",
			toolName: chattool.ExecuteToolName,
			input:    `{"command":"make test","timeout":"soon"}`,
		},
		{
			name:     "ExecuteUnparsableArgs",
			toolName: chattool.ExecuteToolName,
			input:    `{"command":1}`,
		},
		{
			// File tools start no process, so a cancel never stops one.
			name:        "EditFiles",
			toolName:    chattool.EditFilesToolName,
			input:       `{"files":[]}`,
			wantCancel:  true,
			resp:        workspacesdk.CancelToolCallResponse{Started: true, StatusCode: http.StatusOK, ContentType: "application/json", Body: []byte(`{"files":[]}`)},
			wantContent: `{"files":[],"ok":true}`,
		},
		{
			name:        "WriteFile",
			toolName:    chattool.WriteFileToolName,
			input:       `{"path":"/a.txt","content":"b"}`,
			wantCancel:  true,
			resp:        workspacesdk.CancelToolCallResponse{Started: true, StatusCode: http.StatusOK, ContentType: "application/json", Body: []byte(`{"message":"ok"}`)},
			wantContent: `{"ok":true}`,
		},
		{
			name:     "OtherTool",
			toolName: "wait_agent",
			input:    `{}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			call, ok := chattool.NewInterruptedCall(tt.toolName, tt.input)
			require.Equal(t, tt.wantCancel, ok)
			if !ok {
				return
			}
			identity := newInterruptIdentity(t)
			conn := agentconnmock.NewMockAgentConn(gomock.NewController(t))
			conn.EXPECT().CancelToolCall(gomock.Any(), identity.UUID(), workspacesdk.CancelToolCallRequest{StopIfRunAgeBelowMs: tt.wantStop.Milliseconds()}).
				DoAndReturn(func(ctx context.Context, _ string, _ workspacesdk.CancelToolCallRequest) (workspacesdk.CancelToolCallResponse, error) {
					_, ok := workspacesdk.ToolCallFromContext(ctx)
					assert.True(t, ok, "cancel request must carry the tool call")
					return tt.resp, nil
				})

			resp, ok := call.Interrupt(testutil.Context(t, testutil.WaitShort), conn, identity)
			require.True(t, ok)
			// Execute results are never error results, so the model sees
			// every field.
			assert.False(t, resp.IsError, resp.Content)
			assert.JSONEq(t, tt.wantContent, resp.Content)
		})
	}
}

// TestInterruptedCallUnreachable covers the tool response of each tool
// when no connection to the workspace agent could be made. A stopped
// workspace keeps its disk but not its processes.
func TestInterruptedCallUnreachable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		toolName string
		input    string
		err      error
		wantOK   bool
		// wantError is contained in the execute result's error or in the
		// file tool's error text.
		wantError string
	}{
		{name: "ExecuteNoAgent", toolName: chattool.ExecuteToolName, input: `{"command":"make test"}`, err: chattool.ErrWorkspaceHasNoAgent},
		{name: "ExecuteDialFailed", toolName: chattool.ExecuteToolName, input: `{"command":"make test"}`, err: context.DeadlineExceeded, wantOK: true, wantError: "so the command may still be running"},
		{name: "EditFilesNoAgent", toolName: chattool.EditFilesToolName, input: `{"files":[]}`, err: chattool.ErrWorkspaceHasNoAgent, wantOK: true, wantError: "start_workspace"},
		{name: "WriteFileDeletedWorkspace", toolName: chattool.WriteFileToolName, input: `{"path":"/a.txt","content":"b"}`, err: chattool.ErrWorkspaceDeleted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			call, ok := chattool.NewInterruptedCall(tt.toolName, tt.input)
			require.True(t, ok)
			resp, ok := call.Unreachable(newInterruptIdentity(t), tt.err)
			require.Equal(t, tt.wantOK, ok)
			if !ok {
				return
			}
			if tt.toolName != chattool.ExecuteToolName {
				assert.True(t, resp.IsError)
				assert.Contains(t, resp.Content, tt.wantError)
				return
			}
			assert.False(t, resp.IsError, resp.Content)
			var result chattool.ExecuteResult
			require.NoError(t, json.Unmarshal([]byte(resp.Content), &result))
			assert.Contains(t, result.Error, tt.wantError)
		})
	}
}

// TestInterruptedCallSameCancel covers which calls sharing a provider
// tool call ID, and so one tool call UUID, can share one cancel answer.
func TestInterruptedCallSameCancel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b [2]string
		want bool
	}{
		{name: "SameForeground", a: [2]string{chattool.ExecuteToolName, `{"command":"a","timeout":"2h"}`}, b: [2]string{chattool.ExecuteToolName, `{"command":"b","timeout":"2h"}`}, want: true},
		{name: "ForegroundAndBackground", a: [2]string{chattool.ExecuteToolName, `{"command":"a","timeout":"2h"}`}, b: [2]string{chattool.ExecuteToolName, `{"command":"a","run_in_background":true}`}},
		{name: "DifferentTimeouts", a: [2]string{chattool.ExecuteToolName, `{"command":"a","timeout":"2h"}`}, b: [2]string{chattool.ExecuteToolName, `{"command":"a","timeout":"1h"}`}},
		{name: "SameFileTool", a: [2]string{chattool.EditFilesToolName, `{"files":[]}`}, b: [2]string{chattool.EditFilesToolName, `{"files":[{"path":"/a"}]}`}, want: true},
		// Both have a zero stop run age, but their answers become
		// different results.
		{name: "EditAndWrite", a: [2]string{chattool.EditFilesToolName, `{"files":[]}`}, b: [2]string{chattool.WriteFileToolName, `{"path":"/a","content":"b"}`}},
		{name: "EditAndBackgroundExecute", a: [2]string{chattool.EditFilesToolName, `{"files":[]}`}, b: [2]string{chattool.ExecuteToolName, `{"command":"a","run_in_background":true}`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, ok := chattool.NewInterruptedCall(tt.a[0], tt.a[1])
			require.True(t, ok)
			b, ok := chattool.NewInterruptedCall(tt.b[0], tt.b[1])
			require.True(t, ok)
			assert.Equal(t, tt.want, a.SameCancel(b))
			assert.Equal(t, tt.want, b.SameCancel(a))
		})
	}
}
