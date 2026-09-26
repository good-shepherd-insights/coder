package chattool_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/coderd/x/chatd/chattool"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/codersdk/workspacesdk"
	"github.com/coder/coder/v2/codersdk/workspacesdk/agentconnmock"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/quartz"
)

func newInterruptIdentity(t *testing.T) chattool.ToolCallIdentity {
	t.Helper()
	dbNow := time.Now()
	return chattool.ToolCallIdentity{
		ChatID:     uuid.New(),
		MessageID:  42,
		ToolCallID: "call_" + uuid.NewString(),
		Age:        chattool.NewToolCallAge(quartz.NewMock(t), dbNow, dbNow.Add(-time.Minute)),
	}
}

func parseExecuteArgs(t *testing.T, input string) chattool.ExecuteArgs {
	t.Helper()
	var args chattool.ExecuteArgs
	require.NoError(t, json.Unmarshal([]byte(input), &args))
	return args
}

// TestInterruptExecute covers the result of an interrupted execute call
// for every answer to its cancel request.
func TestInterruptExecute(t *testing.T) {
	t.Parallel()

	const (
		foreground = `{"command":"make test","timeout":"2h"}`
		background = `{"command":"make dev","run_in_background":true}`
	)
	intPtr := func(v int) *int { return &v }
	toolCallError := func(code workspacesdk.ToolCallErrorCode) error {
		return &workspacesdk.ToolCallError{Response: codersdk.Response{Message: "refused"}, Code: code}
	}
	tests := []struct {
		name  string
		input string
		// wantStop is the stop_if_run_age_below_ms the request must carry.
		wantStop int64
		resp     workspacesdk.CancelToolCallResponse
		err      error
		// generic is set when the caller keeps its interrupted result.
		generic bool
		// want is compared without Error, which must contain each of
		// wantError. wantBackground sets BackgroundProcessID to the
		// tool call UUID.
		want           chattool.ExecuteResult
		wantError      []string
		wantUUID       bool
		wantBackground bool
	}{
		{
			name:      "ForegroundNotStarted",
			input:     foreground,
			wantStop:  (2 * time.Hour).Milliseconds(),
			wantError: []string{"not run: the command was canceled before the workspace agent received it."},
		},
		{
			name:      "BackgroundNotStarted",
			input:     background,
			wantError: []string{"not run: the command was canceled before the workspace agent received it."},
		},
		{
			name:     "ForegroundCanceled",
			input:    foreground,
			wantStop: (2 * time.Hour).Milliseconds(),
			resp: workspacesdk.CancelToolCallResponse{Started: true, Process: &workspacesdk.ToolCallProcess{
				Canceled: true, Output: "partial", ExitCode: intPtr(137), RunAgeMs: 12_000,
			}},
			want:      chattool.ExecuteResult{Output: "partial", ExitCode: 137, WallDurationMs: 12_000},
			wantError: []string{"canceled by the user after 12s."},
		},
		{
			name:     "ForegroundCanceledWithoutExitCode",
			input:    foreground,
			wantStop: (2 * time.Hour).Milliseconds(),
			resp: workspacesdk.CancelToolCallResponse{Started: true, Process: &workspacesdk.ToolCallProcess{
				Canceled: true, Output: "partial", RunAgeMs: 1_500,
			}},
			want:      chattool.ExecuteResult{Output: "partial", ExitCode: -1, WallDurationMs: 1_500},
			wantError: []string{"canceled by the user after 1.5s."},
		},
		{
			name:     "ForegroundExited",
			input:    foreground,
			wantStop: (2 * time.Hour).Milliseconds(),
			resp: workspacesdk.CancelToolCallResponse{Started: true, Process: &workspacesdk.ToolCallProcess{
				Output: "PASS", ExitCode: intPtr(0), RunAgeMs: 3_000,
			}},
			want: chattool.ExecuteResult{Success: true, Output: "PASS", WallDurationMs: 3_000},
		},
		{
			name:     "ForegroundExitedWithFailure",
			input:    foreground,
			wantStop: (2 * time.Hour).Milliseconds(),
			resp: workspacesdk.CancelToolCallResponse{Started: true, Process: &workspacesdk.ToolCallProcess{
				Output: "FAIL", ExitCode: intPtr(2), RunAgeMs: 3_000,
			}},
			want: chattool.ExecuteResult{Output: "FAIL", ExitCode: 2, WallDurationMs: 3_000},
		},
		{
			// The agent left the process running because its run age
			// reached the timeout: it is past its execute deadline.
			name:     "ForegroundLeftRunning",
			input:    foreground,
			wantStop: (2 * time.Hour).Milliseconds(),
			resp: workspacesdk.CancelToolCallResponse{Started: true, Process: &workspacesdk.ToolCallProcess{
				Running: true, Output: "still going", RunAgeMs: (3 * time.Hour).Milliseconds(),
			}},
			want:           chattool.ExecuteResult{Output: "still going", ExitCode: -1, WallDurationMs: (3 * time.Hour).Milliseconds()},
			wantError:      []string{"command timed out after 2h0m0s"},
			wantBackground: true,
		},
		{
			name:     "ForegroundDefaultTimeout",
			input:    `{"command":"make test"}`,
			wantStop: chattool.ExecuteDefaultTimeout.Milliseconds(),
			resp:     workspacesdk.CancelToolCallResponse{},
			wantError: []string{
				"not run: the command was canceled before the workspace agent received it.",
			},
		},
		{
			name:  "BackgroundRunning",
			input: background,
			resp: workspacesdk.CancelToolCallResponse{Started: true, Process: &workspacesdk.ToolCallProcess{
				Running: true, RunAgeMs: 60_000,
			}},
			want:           chattool.ExecuteResult{Success: true, Backgrounded: true},
			wantBackground: true,
		},
		{
			name:  "BackgroundExited",
			input: background,
			resp: workspacesdk.CancelToolCallResponse{Started: true, Process: &workspacesdk.ToolCallProcess{
				ExitCode: intPtr(0), RunAgeMs: 60_000,
			}},
			want:           chattool.ExecuteResult{Success: true, Backgrounded: true},
			wantBackground: true,
		},
		{
			name:     "ForegroundStartFailed",
			input:    foreground,
			wantStop: (2 * time.Hour).Milliseconds(),
			resp: workspacesdk.CancelToolCallResponse{
				Started:     true,
				StatusCode:  http.StatusInternalServerError,
				ContentType: "application/json",
				Body:        []byte(`{"message":"Failed to start process.","detail":"fork failed"}`),
			},
			wantError: []string{"start process: Failed to start process.: fork failed"},
		},
		{
			name:  "BackgroundStartFailed",
			input: background,
			resp: workspacesdk.CancelToolCallResponse{
				Started:    true,
				StatusCode: http.StatusInternalServerError,
				Body:       []byte("not json"),
			},
			wantError: []string{"start background process: Internal Server Error"},
		},
		{
			name:      "AgentStartedAfterToolCall",
			input:     foreground,
			wantStop:  (2 * time.Hour).Milliseconds(),
			err:       toolCallError(workspacesdk.ToolCallErrorAgentStartedAfterToolCall),
			wantError: []string{"outcome unknown: the workspace agent restarted after this tool call", "Check the workspace state"},
		},
		{
			name:     "OldAgentWithoutCancelRoute",
			input:    foreground,
			wantStop: (2 * time.Hour).Milliseconds(),
			err:      codersdk.NewTestError(http.StatusNotFound, http.MethodPost, "/api/v0/tool-calls/x/cancel"),
			generic:  true,
		},
		{
			name:     "StaleToolCall",
			input:    foreground,
			wantStop: (2 * time.Hour).Milliseconds(),
			err:      toolCallError(workspacesdk.ToolCallErrorStale),
			generic:  true,
		},
		{
			name:     "ToolCallCanceled",
			input:    background,
			err:      toolCallError(workspacesdk.ToolCallErrorCanceled),
			generic:  true,
			wantStop: 0,
		},
		{
			name:      "ForegroundUnreachable",
			input:     foreground,
			wantStop:  (2 * time.Hour).Milliseconds(),
			err:       &url.Error{Op: http.MethodPost, URL: "http://agent", Err: xerrors.New("connection reset by peer")},
			wantError: []string{"outcome unknown: the workspace agent could not be reached", "the command may still be running"},
			wantUUID:  true,
		},
		{
			name:      "BackgroundUnreachable",
			input:     background,
			err:       &url.Error{Op: http.MethodPost, URL: "http://agent", Err: xerrors.New("connection reset by peer")},
			wantError: []string{"outcome unknown: the workspace agent could not be reached", "the command may be running in the background"},
			wantUUID:  true,
		},
		{
			name:      "Unreadable",
			input:     foreground,
			wantStop:  (2 * time.Hour).Milliseconds(),
			err:       xerrors.New("unexpected EOF"),
			wantError: []string{"outcome unknown: the workspace agent's answer could not be read"},
			wantUUID:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := testutil.Context(t, testutil.WaitShort)
			identity := newInterruptIdentity(t)
			conn := agentconnmock.NewMockAgentConn(gomock.NewController(t))
			conn.EXPECT().CancelToolCall(gomock.Any(), identity.UUID(), workspacesdk.CancelToolCallRequest{StopIfRunAgeBelowMs: tt.wantStop}).
				DoAndReturn(func(ctx context.Context, _ string, _ workspacesdk.CancelToolCallRequest) (workspacesdk.CancelToolCallResponse, error) {
					tc, ok := workspacesdk.ToolCallFromContext(ctx)
					if assert.True(t, ok, "cancel request must carry the tool call") {
						assert.Equal(t, identity.MessageID, tc.MessageID)
						assert.Equal(t, identity.ToolCallID, tc.ID)
					}
					return tt.resp, tt.err
				})

			result, ok := chattool.InterruptExecute(ctx, conn, identity, parseExecuteArgs(t, tt.input))
			if tt.generic {
				assert.False(t, ok)
				return
			}
			require.True(t, ok)
			for _, want := range tt.wantError {
				assert.Contains(t, result.Error, want)
			}
			if len(tt.wantError) == 0 {
				assert.Empty(t, result.Error)
			}
			if tt.wantUUID {
				assert.Contains(t, result.Error, identity.UUID())
			}
			want := tt.want
			if tt.wantBackground {
				want.BackgroundProcessID = identity.UUID()
			}
			result.Error = ""
			assert.Equal(t, want, result)
		})
	}
}

// TestInterruptExecuteInvalidTimeout covers a call for which the tool
// starts no process: nothing is sent and the caller keeps its result.
func TestInterruptExecuteInvalidTimeout(t *testing.T) {
	t.Parallel()

	conn := agentconnmock.NewMockAgentConn(gomock.NewController(t))
	_, ok := chattool.InterruptExecute(testutil.Context(t, testutil.WaitShort), conn, newInterruptIdentity(t),
		parseExecuteArgs(t, `{"command":"make test","timeout":"soon"}`))
	assert.False(t, ok)
}

// TestInterruptExecuteUnreachable covers an interrupted execute call
// when no workspace connection could be made.
func TestInterruptExecuteUnreachable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		err   error
		// wantEffect is empty when the caller keeps its result.
		wantEffect string
	}{
		{name: "NoWorkspace", input: `{"command":"make test"}`, err: chattool.ErrChatHasNoWorkspace},
		{name: "WorkspaceDeleted", input: `{"command":"make test"}`, err: xerrors.Errorf("load: %w", chattool.ErrWorkspaceDeleted)},
		{name: "NoAgent", input: `{"command":"make dev &"}`, err: chattool.ErrWorkspaceHasNoAgent},
		{name: "Foreground", input: `{"command":"make test"}`, err: xerrors.New("dial failed"), wantEffect: "the command may still be running"},
		{name: "Background", input: `{"command":"make dev &"}`, err: xerrors.New("dial failed"), wantEffect: "the command may be running in the background"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			identity := newInterruptIdentity(t)
			result, ok := chattool.InterruptExecuteUnreachable(identity, parseExecuteArgs(t, tt.input), tt.err)
			if tt.wantEffect == "" {
				assert.False(t, ok)
				return
			}
			require.True(t, ok)
			assert.Contains(t, result.Error, "outcome unknown: the workspace agent could not be reached (dial failed)")
			assert.Contains(t, result.Error, tt.wantEffect)
			assert.Contains(t, result.Error, identity.UUID())
		})
	}
}

// TestInterruptExecuteBeforeStart shows US10 at the agent boundary: the
// cancel and a later start of the same tool call carry the same tool call
// identity and UUID, so an agent that recorded the cancel refuses the
// start with tool_call_canceled and no process runs, foreground or
// background.
func TestInterruptExecuteBeforeStart(t *testing.T) {
	t.Parallel()

	for _, input := range []string{`{"command":"make test"}`, `{"command":"make dev","run_in_background":true}`} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			ctx := testutil.Context(t, testutil.WaitShort)
			identity := newInterruptIdentity(t)
			key := func(ctx context.Context) string {
				tc, ok := workspacesdk.ToolCallFromContext(ctx)
				require.True(t, ok, "request must carry the tool call")
				return workspacesdk.ToolCallUUID(identity.ChatID, tc.MessageID, tc.ID).String()
			}
			// A minimal agent: a cancel for a tool call it never received
			// records it as canceled, and a later start is refused.
			var mu sync.Mutex
			canceled := make(map[string]bool)
			conn := agentconnmock.NewMockAgentConn(gomock.NewController(t))
			conn.EXPECT().CancelToolCall(gomock.Any(), gomock.Any(), gomock.Any()).
				DoAndReturn(func(ctx context.Context, id string, _ workspacesdk.CancelToolCallRequest) (workspacesdk.CancelToolCallResponse, error) {
					assert.Equal(t, key(ctx), id)
					mu.Lock()
					canceled[id] = true
					mu.Unlock()
					return workspacesdk.CancelToolCallResponse{}, nil
				})
			conn.EXPECT().StartProcess(gomock.Any(), gomock.Any()).
				DoAndReturn(func(ctx context.Context, _ workspacesdk.StartProcessRequest) (workspacesdk.StartProcessResponse, error) {
					mu.Lock()
					defer mu.Unlock()
					if canceled[key(ctx)] {
						return workspacesdk.StartProcessResponse{}, &workspacesdk.ToolCallError{
							Response: codersdk.Response{Message: "tool call was canceled"},
							Code:     workspacesdk.ToolCallErrorCanceled,
						}
					}
					t.Error("start was not refused")
					return workspacesdk.StartProcessResponse{ID: key(ctx), Started: true}, nil
				})

			result, ok := chattool.InterruptExecute(ctx, conn, identity, parseExecuteArgs(t, input))
			require.True(t, ok)
			assert.Contains(t, result.Error, "not run: the command was canceled before the workspace agent received it.")

			// A late attempt of the same tool call, as a generation
			// goroutine that has not seen the interrupt yet sends it.
			resp, err := newExecuteTool(t, conn).Run(chattool.WithToolCallIdentity(ctx, identity), fantasy.ToolCall{
				ID:    identity.ToolCallID,
				Name:  chattool.ExecuteToolName,
				Input: input,
			})
			require.NoError(t, err)
			assert.Contains(t, resp.Content, string(workspacesdk.ToolCallErrorCanceled))
		})
	}
}
