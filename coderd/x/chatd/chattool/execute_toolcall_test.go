package chattool_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
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

// TestExecuteToolCall covers execute when the dispatch identifies the
// tool call: the start request carries the tool call, a process the
// agent started earlier is waited on for what remains of its timeout,
// and agent refusals map to results.
func TestExecuteToolCall(t *testing.T) {
	t.Parallel()

	const toolCallAge = 30 * time.Second
	exitCode := func(code int) *int { return &code }
	toolCallError := func(code workspacesdk.ToolCallErrorCode) error {
		return &workspacesdk.ToolCallError{
			Response: codersdk.Response{Message: "refused"},
			Code:     code,
		}
	}
	transportErr := &url.Error{Op: "Post", URL: "http://agent/api/v0/processes/start", Err: xerrors.New("connection reset by peer")}
	agentErrorResponse := codersdk.NewError(http.StatusInternalServerError, codersdk.Response{
		Message: "Failed to start process.",
		Detail:  "no such directory",
	})

	// outputCall is one expected ProcessOutput call, in order. remaining
	// is the wait deadline the call must see; zero skips the check.
	type outputCall struct {
		wait      bool
		remaining time.Duration
		resp      workspacesdk.ProcessOutputResponse
		err       error
	}
	tests := []struct {
		name string
		// noIdentity runs the tool without a tool call identity.
		noIdentity bool
		input      string
		// start answers StartProcess given the tool call's process ID.
		start   func(processID string) (workspacesdk.StartProcessResponse, error)
		outputs []outputCall
		check   func(t *testing.T, result chattool.ExecuteResult, processID string)
	}{
		{
			name:       "NoIdentity",
			noIdentity: true,
			input:      `{"command":"make test","timeout":"10m"}`,
			start: func(string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{ID: "proc-1"}, nil
			},
			outputs: []outputCall{{wait: true, remaining: 10 * time.Minute, resp: workspacesdk.ProcessOutputResponse{ExitCode: exitCode(0), Output: "ok"}}},
			check: func(t *testing.T, result chattool.ExecuteResult, _ string) {
				assert.True(t, result.Success)
				assert.Equal(t, "ok", result.Output)
			},
		},
		{
			name:  "Started",
			input: `{"command":"make test","timeout":"10m"}`,
			start: func(processID string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{ID: processID, Started: true}, nil
			},
			outputs: []outputCall{{wait: true, remaining: 10 * time.Minute, resp: workspacesdk.ProcessOutputResponse{ExitCode: exitCode(0), Output: "ok"}}},
			check: func(t *testing.T, result chattool.ExecuteResult, _ string) {
				assert.True(t, result.Success)
				assert.Equal(t, "ok", result.Output)
				assert.Less(t, result.WallDurationMs, testutil.WaitShort.Milliseconds())
			},
		},
		{
			name:  "AttachWaitsForRemainingTimeout",
			input: `{"command":"make test","timeout":"10m"}`,
			start: func(processID string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{ID: processID, Started: true, RunAge: 4 * time.Minute}, nil
			},
			outputs: []outputCall{{wait: true, remaining: 6 * time.Minute, resp: workspacesdk.ProcessOutputResponse{ExitCode: exitCode(2), Output: "FAIL"}}},
			check: func(t *testing.T, result chattool.ExecuteResult, _ string) {
				assert.False(t, result.Success)
				assert.Equal(t, 2, result.ExitCode)
				assert.Equal(t, "FAIL", result.Output)
				assert.Empty(t, result.BackgroundProcessID)
				assert.GreaterOrEqual(t, result.WallDurationMs, (4 * time.Minute).Milliseconds())
				assert.Less(t, result.WallDurationMs, (4*time.Minute + testutil.WaitShort).Milliseconds())
			},
		},
		{
			// The agent caps each blocking wait, so a wait that returns
			// with the process running is repeated until it exits.
			name:  "AttachWaitsAgainAfterAgentWaitCap",
			input: `{"command":"make test","timeout":"10m"}`,
			start: func(processID string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{ID: processID, Started: true, RunAge: 4 * time.Minute}, nil
			},
			outputs: []outputCall{
				{wait: true, remaining: 6 * time.Minute, resp: workspacesdk.ProcessOutputResponse{Running: true, Output: "partial"}},
				{wait: true, remaining: 6 * time.Minute, resp: workspacesdk.ProcessOutputResponse{ExitCode: exitCode(0), Output: "done"}},
			},
			check: func(t *testing.T, result chattool.ExecuteResult, _ string) {
				assert.True(t, result.Success)
				assert.Equal(t, "done", result.Output)
			},
		},
		{
			// A negative age from a faulty agent cannot extend the wait
			// past the timeout.
			name:  "AttachNegativeAgeWaitsTimeout",
			input: `{"command":"make test","timeout":"10m"}`,
			start: func(processID string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{ID: processID, Started: true, RunAge: -time.Minute}, nil
			},
			outputs: []outputCall{{wait: true, remaining: 10 * time.Minute, resp: workspacesdk.ProcessOutputResponse{ExitCode: exitCode(0), Output: "ok"}}},
			check: func(t *testing.T, result chattool.ExecuteResult, _ string) {
				assert.True(t, result.Success)
				assert.GreaterOrEqual(t, result.WallDurationMs, int64(0))
			},
		},
		{
			name:  "AttachNoTimeoutLeftRunning",
			input: `{"command":"make test","timeout":"10m"}`,
			start: func(processID string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{ID: processID, Started: true, RunAge: 10 * time.Minute}, nil
			},
			outputs: []outputCall{{resp: workspacesdk.ProcessOutputResponse{Running: true, Output: "partial"}}},
			check: func(t *testing.T, result chattool.ExecuteResult, processID string) {
				assert.False(t, result.Success)
				assert.Equal(t, -1, result.ExitCode)
				assert.Equal(t, "command timed out after 10m0s", result.Error)
				assert.Equal(t, "partial", result.Output)
				assert.Equal(t, processID, result.BackgroundProcessID)
				assert.GreaterOrEqual(t, result.WallDurationMs, (10 * time.Minute).Milliseconds())
			},
		},
		{
			name:  "AttachPastTimeoutExited",
			input: `{"command":"make test","timeout":"10m"}`,
			start: func(processID string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{ID: processID, Started: true, RunAge: 12 * time.Minute}, nil
			},
			outputs: []outputCall{{resp: workspacesdk.ProcessOutputResponse{ExitCode: exitCode(3), Output: "exited"}}},
			check: func(t *testing.T, result chattool.ExecuteResult, _ string) {
				assert.False(t, result.Success)
				assert.Equal(t, 3, result.ExitCode)
				assert.Equal(t, "exited", result.Output)
				assert.Empty(t, result.Error)
				assert.Empty(t, result.BackgroundProcessID)
			},
		},
		{
			name:  "AttachNoTimeoutLeftSnapshotFails",
			input: `{"command":"make test","timeout":"10m"}`,
			start: func(processID string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{ID: processID, Started: true, RunAge: 10 * time.Minute}, nil
			},
			outputs: []outputCall{{err: xerrors.New("connection reset")}},
			check: func(t *testing.T, result chattool.ExecuteResult, processID string) {
				assert.False(t, result.Success)
				assert.Equal(t, -1, result.ExitCode)
				// The wait was cut at the deadline, so the process may have
				// exited before it; the result must not claim a timeout.
				assert.Contains(t, result.Error, "outcome unknown")
				assert.Contains(t, result.Error, "not received within 10m0s")
				assert.Contains(t, result.Error, "connection reset")
				assert.Contains(t, result.Error, processID)
				assert.NotContains(t, result.Error, "timed out")
				assert.Equal(t, processID, result.BackgroundProcessID)
			},
		},
		{
			// An agent without tool call support picks its own process
			// ID, so execute behaves as it does without an identity.
			name:  "OldAgent",
			input: `{"command":"make test","timeout":"10m"}`,
			start: func(string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{ID: uuid.NewString(), Started: true, RunAge: 4 * time.Minute}, nil
			},
			outputs: []outputCall{{wait: true, remaining: 10 * time.Minute, resp: workspacesdk.ProcessOutputResponse{ExitCode: exitCode(0), Output: "ok"}}},
			check: func(t *testing.T, result chattool.ExecuteResult, _ string) {
				assert.True(t, result.Success)
				assert.Less(t, result.WallDurationMs, testutil.WaitShort.Milliseconds())
			},
		},
		{
			name:  "AgentStartedAfterToolCall",
			input: `{"command":"make test"}`,
			start: func(string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{}, toolCallError(workspacesdk.ToolCallErrorAgentStartedAfterToolCall)
			},
			check: func(t *testing.T, result chattool.ExecuteResult, _ string) {
				assert.False(t, result.Success)
				assert.Contains(t, result.Error, "workspace agent restarted after this tool call")
				assert.Contains(t, result.Error, "may have run")
				assert.Empty(t, result.BackgroundProcessID)
			},
		},
		{
			name:  "AgentStartedAfterToolCallBackground",
			input: `{"command":"make dev","run_in_background":true}`,
			start: func(string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{}, toolCallError(workspacesdk.ToolCallErrorAgentStartedAfterToolCall)
			},
			check: func(t *testing.T, result chattool.ExecuteResult, _ string) {
				assert.False(t, result.Success)
				assert.Contains(t, result.Error, "workspace agent restarted after this tool call")
				assert.Contains(t, result.Error, "may have run")
				assert.False(t, result.Backgrounded)
			},
		},
		{
			name:  "InputMismatch",
			input: `{"command":"make test"}`,
			start: func(string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{}, toolCallError(workspacesdk.ToolCallErrorInputMismatch)
			},
			check: func(t *testing.T, result chattool.ExecuteResult, processID string) {
				assert.False(t, result.Success)
				assert.Contains(t, result.Error, "changed nothing")
				assert.Contains(t, result.Error, "already exists with a different input")
				assert.Contains(t, result.Error, processID)
			},
		},
		{
			// The request may have reached the agent, so the command may
			// have started.
			name:  "Unreachable",
			input: `{"command":"make test"}`,
			start: func(string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{}, xerrors.Errorf("do request: %w", transportErr)
			},
			check: func(t *testing.T, result chattool.ExecuteResult, processID string) {
				assert.False(t, result.Success)
				assert.Contains(t, result.Error, "outcome unknown")
				assert.Contains(t, result.Error, "could not be reached")
				assert.Contains(t, result.Error, "may have started")
				assert.Contains(t, result.Error, "connection reset by peer")
				assert.Contains(t, result.Error, "process_output")
				assert.Contains(t, result.Error, processID)
				assert.NotContains(t, result.Error, "support tool calls")
				assert.NotContains(t, result.Error, "was sent")
			},
		},
		{
			name:  "UnreachableBackground",
			input: `{"command":"make dev","run_in_background":true}`,
			start: func(string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{}, xerrors.Errorf("do request: %w", transportErr)
			},
			check: func(t *testing.T, result chattool.ExecuteResult, processID string) {
				assert.False(t, result.Success)
				assert.Contains(t, result.Error, "outcome unknown")
				assert.Contains(t, result.Error, processID)
				assert.False(t, result.Backgrounded)
			},
		},
		{
			// The agent answered, so the process may have started.
			name:  "UnreadableResponse",
			input: `{"command":"make test"}`,
			start: func(string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{}, xerrors.New("decode response: unexpected EOF")
			},
			check: func(t *testing.T, result chattool.ExecuteResult, processID string) {
				assert.False(t, result.Success)
				assert.Contains(t, result.Error, "outcome unknown")
				assert.Contains(t, result.Error, "could not be read")
				assert.Contains(t, result.Error, "unexpected EOF")
				assert.Contains(t, result.Error, "may have started")
				assert.Contains(t, result.Error, processID)
			},
		},
		{
			// The agent answered, so the error text is today's.
			name:  "AgentErrorResponse",
			input: `{"command":"make test"}`,
			start: func(string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{}, agentErrorResponse
			},
			check: func(t *testing.T, result chattool.ExecuteResult, _ string) {
				assert.False(t, result.Success)
				assert.Equal(t, "start process: "+agentErrorResponse.Error(), result.Error)
			},
		},
		{
			name:  "StaleToolCall",
			input: `{"command":"make test"}`,
			start: func(string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{}, toolCallError(workspacesdk.ToolCallErrorStale)
			},
			check: func(t *testing.T, result chattool.ExecuteResult, _ string) {
				assert.False(t, result.Success)
				assert.Contains(t, result.Error, string(workspacesdk.ToolCallErrorStale))
			},
		},
		{
			name:  "ToolCallCanceled",
			input: `{"command":"make test"}`,
			start: func(string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{}, toolCallError(workspacesdk.ToolCallErrorCanceled)
			},
			check: func(t *testing.T, result chattool.ExecuteResult, _ string) {
				assert.False(t, result.Success)
				assert.Contains(t, result.Error, string(workspacesdk.ToolCallErrorCanceled))
			},
		},
		{
			name:  "Background",
			input: `{"command":"make dev","run_in_background":true}`,
			start: func(processID string) (workspacesdk.StartProcessResponse, error) {
				return workspacesdk.StartProcessResponse{ID: processID, Started: true, RunAge: 4 * time.Minute}, nil
			},
			check: func(t *testing.T, result chattool.ExecuteResult, processID string) {
				assert.True(t, result.Success)
				assert.True(t, result.Backgrounded)
				assert.Equal(t, processID, result.BackgroundProcessID)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// The wait deadline checks need a parent without a deadline.
			// No mock call blocks, so the run cannot hang.
			ctx := context.WithoutCancel(testutil.Context(t, testutil.WaitMedium))
			dbNow := time.Now()
			identity := chattool.ToolCallIdentity{
				ChatID:     uuid.New(),
				MessageID:  42,
				ToolCallID: "call_" + uuid.NewString(),
				Age:        chattool.NewToolCallAge(quartz.NewMock(t), dbNow, dbNow.Add(-toolCallAge)),
			}
			processID := workspacesdk.ToolCallUUID(identity.ChatID, identity.MessageID, identity.ToolCallID).String()
			runCtx := ctx
			if !tt.noIdentity {
				runCtx = chattool.WithToolCallIdentity(ctx, identity)
			}

			ctrl := gomock.NewController(t)
			mockConn := agentconnmock.NewMockAgentConn(ctrl)
			var startResp workspacesdk.StartProcessResponse
			mockConn.EXPECT().
				StartProcess(gomock.Any(), gomock.Any()).
				DoAndReturn(func(ctx context.Context, _ workspacesdk.StartProcessRequest) (workspacesdk.StartProcessResponse, error) {
					tc, ok := workspacesdk.ToolCallFromContext(ctx)
					if tt.noIdentity {
						assert.False(t, ok, "start request must not carry a tool call")
					} else {
						assert.True(t, ok, "start request must carry the tool call")
						assert.Equal(t, workspacesdk.ToolCall{MessageID: 42, ID: identity.ToolCallID, Age: toolCallAge}, tc)
					}
					resp, err := tt.start(processID)
					startResp = resp
					return resp, err
				})
			var calls []any
			for _, want := range tt.outputs {
				calls = append(calls, mockConn.EXPECT().
					ProcessOutput(gomock.Any(), gomock.Any(), gomock.Any()).
					DoAndReturn(func(ctx context.Context, id string, opts *workspacesdk.ProcessOutputOptions) (workspacesdk.ProcessOutputResponse, error) {
						assert.Equal(t, startResp.ID, id)
						_, ok := workspacesdk.ToolCallFromContext(ctx)
						assert.False(t, ok, "only the start request carries the tool call")
						assert.Equal(t, want.wait, opts != nil && opts.Wait)
						if want.remaining > 0 {
							deadline, ok := ctx.Deadline()
							require.True(t, ok)
							remaining := time.Until(deadline)
							assert.LessOrEqual(t, remaining, want.remaining)
							assert.Greater(t, remaining, want.remaining-testutil.WaitShort)
						}
						return want.resp, want.err
					}))
			}
			gomock.InOrder(calls...)

			resp, err := newExecuteTool(t, mockConn).Run(runCtx, fantasy.ToolCall{
				ID:    identity.ToolCallID,
				Name:  "execute",
				Input: tt.input,
			})
			require.NoError(t, err)
			var result chattool.ExecuteResult
			require.NoError(t, json.Unmarshal([]byte(resp.Content), &result), resp.Content)
			tt.check(t, result, processID)
		})
	}
}

// TestExecuteToolCallNoConnection covers execute when the workspace
// connection cannot be obtained. With a tool call identity, an earlier
// attempt may have started the command.
func TestExecuteToolCallNoConnection(t *testing.T) {
	t.Parallel()

	dialErr := xerrors.New("dial workspace agent: connection refused")
	tests := []struct {
		name       string
		noIdentity bool
		connErr    error
		check      func(t *testing.T, resp fantasy.ToolResponse, processID string)
	}{
		{
			name:    "Identity",
			connErr: dialErr,
			check: func(t *testing.T, resp fantasy.ToolResponse, processID string) {
				assert.True(t, resp.IsError)
				assert.Contains(t, resp.Content, "outcome unknown")
				assert.Contains(t, resp.Content, "could not be reached")
				assert.Contains(t, resp.Content, dialErr.Error())
				assert.Contains(t, resp.Content, "earlier attempt may have started the command")
				assert.Contains(t, resp.Content, "process_output")
				assert.Contains(t, resp.Content, processID)
			},
		},
		{
			name:       "NoIdentity",
			noIdentity: true,
			connErr:    dialErr,
			check: func(t *testing.T, resp fantasy.ToolResponse, _ string) {
				assert.True(t, resp.IsError)
				assert.Equal(t, dialErr.Error(), resp.Content)
			},
		},
		{
			// Processes do not outlive the workspace agent, so a
			// workspace without one, or a deleted workspace, settles
			// the outcome.
			name:    "IdentityNoAgent",
			connErr: xerrors.Errorf("get workspace connection: %w", chattool.ErrWorkspaceHasNoAgent),
			check: func(t *testing.T, resp fantasy.ToolResponse, _ string) {
				assert.True(t, resp.IsError)
				assert.Contains(t, resp.Content, chattool.ErrWorkspaceHasNoAgent.Error())
				assert.NotContains(t, resp.Content, "outcome unknown")
			},
		},
		{
			// A chat without a workspace has run nothing.
			name:    "IdentityNoWorkspace",
			connErr: xerrors.Errorf("get workspace connection: %w", chattool.ErrChatHasNoWorkspace),
			check: func(t *testing.T, resp fantasy.ToolResponse, _ string) {
				assert.True(t, resp.IsError)
				assert.Contains(t, resp.Content, chattool.ErrChatHasNoWorkspace.Error())
				assert.NotContains(t, resp.Content, "outcome unknown")
			},
		},
		{
			name:    "IdentityWorkspaceDeleted",
			connErr: xerrors.Errorf("get workspace connection: %w", chattool.ErrWorkspaceDeleted),
			check: func(t *testing.T, resp fantasy.ToolResponse, _ string) {
				assert.True(t, resp.IsError)
				assert.Contains(t, resp.Content, chattool.ErrWorkspaceDeleted.Error())
				assert.NotContains(t, resp.Content, "outcome unknown")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := testutil.Context(t, testutil.WaitShort)
			dbNow := time.Now()
			identity := chattool.ToolCallIdentity{
				ChatID:     uuid.New(),
				MessageID:  42,
				ToolCallID: "call_" + uuid.NewString(),
				Age:        chattool.NewToolCallAge(quartz.NewMock(t), dbNow, dbNow),
			}
			if !tt.noIdentity {
				ctx = chattool.WithToolCallIdentity(ctx, identity)
			}
			tool := chattool.Execute(chattool.ExecuteOptions{
				GetWorkspaceConn: func(context.Context) (workspacesdk.AgentConn, error) {
					return nil, tt.connErr
				},
			})
			resp, err := tool.Run(ctx, fantasy.ToolCall{
				ID:    identity.ToolCallID,
				Name:  "execute",
				Input: `{"command":"make test"}`,
			})
			require.NoError(t, err)
			tt.check(t, resp, workspacesdk.ToolCallUUID(identity.ChatID, identity.MessageID, identity.ToolCallID).String())
		})
	}
}

// TestExecuteToolCallAgentAnswerTimeout covers a start request the
// workspace agent never answers: it ends after chattool.AgentAnswerTimeout
// with an unknown outcome, unless the tool call's own context ended first
// or the execute timeout is shorter.
func TestExecuteToolCallAgentAnswerTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		noIdentity bool
		input      string
		// endTask cancels the tool call's context while the start
		// request waits, instead of advancing the clock.
		endTask bool
		// executeTimeoutFirst leaves the clock alone: the execute
		// timeout ends the start request first.
		executeTimeoutFirst bool
		check               func(t *testing.T, result chattool.ExecuteResult, processID string)
	}{
		{
			name:  "Foreground",
			input: `{"command":"make test","timeout":"20m"}`,
			check: func(t *testing.T, result chattool.ExecuteResult, processID string) {
				assert.False(t, result.Success)
				assert.True(t, strings.HasPrefix(result.Error, "outcome unknown: the workspace agent did not answer within 1m0s, so "), result.Error)
				assert.Contains(t, result.Error, "may have started")
				assert.Contains(t, result.Error, processID)
			},
		},
		{
			name:  "Background",
			input: `{"command":"make dev","run_in_background":true}`,
			check: func(t *testing.T, result chattool.ExecuteResult, processID string) {
				assert.False(t, result.Success)
				assert.False(t, result.Backgrounded)
				assert.True(t, strings.HasPrefix(result.Error, "outcome unknown: the workspace agent did not answer within 1m0s, so "), result.Error)
				assert.Contains(t, result.Error, processID)
			},
		},
		{
			name:       "NoIdentity",
			noIdentity: true,
			input:      `{"command":"make dev","run_in_background":true}`,
			check: func(t *testing.T, result chattool.ExecuteResult, _ string) {
				assert.False(t, result.Success)
				assert.Contains(t, result.Error, "start background process: ")
				assert.Contains(t, result.Error, "did not answer within 1m0s")
				assert.NotContains(t, result.Error, "outcome unknown")
			},
		},
		{
			// Nothing commits the result of an ended task, so it keeps
			// today's text.
			name:    "TaskEnded",
			input:   `{"command":"make dev","run_in_background":true}`,
			endTask: true,
			check: func(t *testing.T, result chattool.ExecuteResult, _ string) {
				assert.False(t, result.Success)
				assert.Contains(t, result.Error, "start background process: ")
				assert.NotContains(t, result.Error, "outcome unknown")
				assert.NotContains(t, result.Error, "did not answer")
			},
		},
		{
			name:                "ExecuteTimeoutFirst",
			input:               `{"command":"make test","timeout":"50ms"}`,
			executeTimeoutFirst: true,
			check: func(t *testing.T, result chattool.ExecuteResult, processID string) {
				assert.False(t, result.Success)
				assert.Contains(t, result.Error, "outcome unknown")
				assert.Contains(t, result.Error, "could not be reached")
				assert.NotContains(t, result.Error, "did not answer")
				assert.Contains(t, result.Error, processID)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			testCtx := testutil.Context(t, testutil.WaitMedium)
			taskCtx, endTask := context.WithCancel(testCtx)
			defer endTask()
			clock := quartz.NewMock(t)
			trap := clock.Trap().AfterFunc("chattool", "agent-answer")
			defer trap.Close()
			dbNow := time.Now()
			identity := chattool.ToolCallIdentity{
				ChatID:     uuid.New(),
				MessageID:  42,
				ToolCallID: "call_" + uuid.NewString(),
				Age:        chattool.NewToolCallAge(clock, dbNow, dbNow),
			}
			runCtx := taskCtx
			if !tt.noIdentity {
				runCtx = chattool.WithToolCallIdentity(taskCtx, identity)
			}

			ctrl := gomock.NewController(t)
			mockConn := agentconnmock.NewMockAgentConn(ctrl)
			// A connected agent that never answers: the request ends only
			// when its context does, as the SDK reports it.
			mockConn.EXPECT().
				StartProcess(gomock.Any(), gomock.Any()).
				DoAndReturn(func(ctx context.Context, _ workspacesdk.StartProcessRequest) (workspacesdk.StartProcessResponse, error) {
					<-ctx.Done()
					return workspacesdk.StartProcessResponse{}, xerrors.Errorf("do request: %w",
						&url.Error{Op: "Post", URL: "http://agent/api/v0/processes/start", Err: ctx.Err()})
				})
			tool := chattool.Execute(chattool.ExecuteOptions{
				GetWorkspaceConn: func(context.Context) (workspacesdk.AgentConn, error) { return mockConn, nil },
				Clock:            clock,
			})
			done := make(chan fantasy.ToolResponse, 1)
			go func() {
				resp, err := tool.Run(runCtx, fantasy.ToolCall{ID: identity.ToolCallID, Name: "execute", Input: tt.input})
				assert.NoError(t, err)
				done <- resp
			}()
			trap.MustWait(testCtx).MustRelease(testCtx)
			switch {
			case tt.endTask:
				endTask()
			case !tt.executeTimeoutFirst:
				clock.Advance(chattool.AgentAnswerTimeout).MustWait(testCtx)
			}
			resp := testutil.RequireReceive(testCtx, t, done)
			var result chattool.ExecuteResult
			require.NoError(t, json.Unmarshal([]byte(resp.Content), &result), resp.Content)
			tt.check(t, result, workspacesdk.ToolCallUUID(identity.ChatID, identity.MessageID, identity.ToolCallID).String())
		})
	}
}
