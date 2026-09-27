package chattool

import (
	"context"
	"encoding/json"
	"time"

	"charm.land/fantasy"

	"github.com/coder/coder/v2/codersdk/workspacesdk"
	"github.com/coder/quartz"
)

// InterruptedCall is an unresolved tool call that interrupt handling
// cancels on the workspace agent: an execute, edit_files, or write_file
// call.
type InterruptedCall struct {
	toolName string
	// args are the arguments of an execute call.
	args ExecuteArgs
	// stopRunAge and background decide the cancel request and how its
	// answer maps to a result.
	stopRunAge time.Duration
	background bool
}

// NewInterruptedCall returns how interrupt handling cancels a call of the
// tool toolName with input. Every edit_files and write_file call
// qualifies with a zero stop run age: they start no process, and cancel
// waits for an edit in progress. ok is false when the call keeps the
// generic interrupted result without a cancel: another tool, unparsable
// execute arguments, or execute arguments for which the tool starts no
// process.
func NewInterruptedCall(toolName, input string) (call InterruptedCall, ok bool) {
	switch toolName {
	case EditFilesToolName, WriteFileToolName:
		return InterruptedCall{toolName: toolName}, true
	case ExecuteToolName:
	default:
		return InterruptedCall{}, false
	}
	var args ExecuteArgs
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return InterruptedCall{}, false
	}
	stopRunAge, ok := args.InterruptStopRunAge()
	if !ok {
		return InterruptedCall{}, false
	}
	return InterruptedCall{
		toolName:   toolName,
		args:       args,
		stopRunAge: stopRunAge,
		background: args.RunsInBackground(),
	}, true
}

// SameCancel reports whether c and other send the same cancel request and
// map its answer to the same result. Calls that share a provider tool
// call ID share one tool call UUID, so they can share one cancel only
// when this holds.
func (c InterruptedCall) SameCancel(other InterruptedCall) bool {
	return c.toolName == other.toolName && c.stopRunAge == other.stopRunAge && c.background == other.background
}

// Interrupt cancels the call id on the workspace agent and returns the
// tool response it gets, built from the agent's answer. clock times the
// wait for the answer (nil means a real clock). ok is false when the
// call keeps the generic interrupted result.
func (c InterruptedCall) Interrupt(ctx context.Context, clock quartz.Clock, conn workspacesdk.AgentConn, id ToolCallIdentity) (resp fantasy.ToolResponse, ok bool) {
	if c.toolName != ExecuteToolName {
		return InterruptFileToolCall(ctx, clock, conn, c.toolName, id)
	}
	result, ok := InterruptExecute(ctx, clock, conn, id, c.args)
	return marshalToolResponse(result), ok
}

// Unreachable returns the tool response of the call id when no
// connection to the workspace agent could be made. ok is false when the
// call keeps the generic interrupted result.
func (c InterruptedCall) Unreachable(id ToolCallIdentity, err error) (resp fantasy.ToolResponse, ok bool) {
	if c.toolName != ExecuteToolName {
		return InterruptFileToolCallUnreachable(c.toolName, id, err)
	}
	result, ok := InterruptExecuteUnreachable(id, c.args, err)
	return marshalToolResponse(result), ok
}
