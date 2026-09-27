package chattool

import (
	"context"
	"errors"
	"fmt"

	"charm.land/fantasy"

	"github.com/coder/coder/v2/codersdk/workspacesdk"
)

// Registered names of the file tools that send tool call headers.
const (
	EditFilesToolName = "edit_files"
	WriteFileToolName = "write_file"
)

// checkFileText tells the model how to learn whether a file tool call
// changed the file.
const checkFileText = "Check the file before changing it again."

// fileChange names what the file tool toolName changes.
func fileChange(toolName string) string {
	if toolName == WriteFileToolName {
		return "write"
	}
	return "edit"
}

// fileToolErrorWords are the words of the file tool toolName in the
// results AgentErrorText builds.
func fileToolErrorWords(toolName string) AgentErrorWords {
	action, existing := "edit files", "an edit for this tool call"
	if toolName == WriteFileToolName {
		action, existing = "write file", "a write for this tool call"
	}
	change := fileChange(toolName)
	return AgentErrorWords{
		Action:          action,
		Existing:        existing,
		Effect:          "the " + change + " may have been applied",
		Check:           checkFileText,
		RestartedEffect: "the " + change + " may have been applied before the restart",
	}
}

// fileToolChangeLost reports whether err, from obtaining the workspace
// connection, means no file tool change can survive: the chat has no
// workspace, or the workspace was deleted. A stopped workspace, which
// has no agent, keeps its disk.
func fileToolChangeLost(err error) bool {
	return errors.Is(err, ErrChatHasNoWorkspace) || errors.Is(err, ErrWorkspaceDeleted)
}

// fileToolConnErrorResult converts a failure to connect to the workspace
// agent into the result of the file tool call toolName in ctx. An earlier
// attempt of the tool call may have applied the change unless no change
// can survive.
func fileToolConnErrorResult(ctx context.Context, toolName string, err error) fantasy.ToolResponse {
	if _, ok := reportableToolCall(ctx); ok && !fileToolChangeLost(err) {
		return fantasy.NewTextErrorResponse(UnknownOutcome(AgentUnreachableReason(err),
			"an earlier attempt may have applied the "+fileChange(toolName), checkFileText))
	}
	return fantasy.NewTextErrorResponse(err.Error())
}

// fileRequestErrorResult converts an edit_files or write_file request
// error into the result of the tool call in ctx when the error depends on
// the tool call: the agent's refusal, or a failure without a readable
// answer. ok is false for other errors, including an error answer from
// the agent, which the tool reports as it always has.
func fileRequestErrorResult(ctx context.Context, toolName string, err error) (result fantasy.ToolResponse, ok bool) {
	if err == nil {
		return fantasy.ToolResponse{}, false
	}
	if _, ok := reportableToolCall(ctx); !ok {
		return fantasy.ToolResponse{}, false
	}
	text, ok := AgentErrorText(err, fileToolErrorWords(toolName))
	if !ok {
		return fantasy.ToolResponse{}, false
	}
	return fantasy.NewTextErrorResponse(text), true
}

// InterruptFileToolCall cancels an edit_files or write_file call, named
// toolName, that the user interrupted and returns its result, built from
// the workspace agent's answer. The agent cannot stop an edit in
// progress, so it waits for it and answers with the recorded response,
// which gives the result the tool returns for it. ok is false when the
// call keeps the caller's generic interrupted result: the agent's answer
// does not describe the tool call (an error answer, including the 404 of
// an agent without the cancel route).
func InterruptFileToolCall(ctx context.Context, conn workspacesdk.AgentConn, toolName string, id ToolCallIdentity) (result fantasy.ToolResponse, ok bool) {
	// A zero stop run age never stops a process; file tools start none.
	resp, err := conn.CancelToolCall(workspacesdk.WithToolCall(ctx, id.AgentToolCall()), id.UUID(),
		workspacesdk.CancelToolCallRequest{StopIfRunAgeBelowMs: 0})
	if err != nil {
		text, ok := AgentErrorText(err, fileToolErrorWords(toolName))
		return fantasy.NewTextErrorResponse(text), ok
	}
	if !resp.Started {
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"not applied: the %s was canceled before the workspace agent received it.", fileChange(toolName))), true
	}
	if toolName == WriteFileToolName {
		return writeFileResult(resp.WriteFileResult()), true
	}
	return editFilesResult(resp.EditFilesResult()), true
}

// InterruptFileToolCallUnreachable returns the result of an edit_files or
// write_file call, named toolName, that the user interrupted when no
// connection to the workspace agent could be made. ok is false when the
// call keeps the caller's generic interrupted result: no change can have
// survived. A stopped workspace keeps its disk, so a call without a
// workspace agent gets an unknown result.
func InterruptFileToolCallUnreachable(toolName string, err error) (result fantasy.ToolResponse, ok bool) {
	if fileToolChangeLost(err) {
		return fantasy.ToolResponse{}, false
	}
	words := fileToolErrorWords(toolName)
	return fantasy.NewTextErrorResponse(UnknownOutcome(AgentUnreachableReason(err), words.Effect, words.Check)), true
}
