package chattool

import (
	"context"
	"strings"

	"charm.land/fantasy"

	"github.com/coder/coder/v2/codersdk/workspacesdk"
	"github.com/coder/quartz"
)

type WriteFileOptions struct {
	GetWorkspaceConn func(context.Context) (workspacesdk.AgentConn, error)
	ResolvePlanPath  func(context.Context) (chatPath string, home string, err error)
	IsPlanTurn       bool
	// Clock times the wait for the agent's answer to the request
	// (AgentAnswerTimeout). Nil means a real clock.
	Clock quartz.Clock
}

type WriteFileArgs struct {
	Path    string `json:"path" description:"Absolute path of the file to write. Plan files must use the chat-specific absolute plan path."`
	Content string `json:"content" description:"Complete file contents. Replaces any existing contents."`
}

func WriteFile(options WriteFileOptions) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		WriteFileToolName,
		"Create a file in the workspace or overwrite an existing one with the given content. "+
			"Use edit_files for targeted changes to an existing file. "+
			"During plan turns, only the chat-specific plan file path is writable.",
		func(ctx context.Context, args WriteFileArgs, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var planPath string
			if options.IsPlanTurn {
				args.Path = strings.TrimSpace(args.Path)
				resolvedPlanPath, err := resolvePlanTurnPath(ctx, options.ResolvePlanPath)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				if args.Path != resolvedPlanPath {
					return fantasy.NewTextErrorResponse("during plan turns, write_file is restricted to " + resolvedPlanPath), nil
				}
				planPath = resolvedPlanPath
			}
			if options.GetWorkspaceConn == nil {
				return fantasy.NewTextErrorResponse("workspace connection resolver is not configured"), nil
			}
			conn, err := options.GetWorkspaceConn(ctx)
			if err != nil {
				return fileToolConnErrorResult(ctx, WriteFileToolName, err), nil
			}
			if planPath != "" {
				if err := ensurePlanPathResolvesToItself(ctx, conn, planPath); err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
			}
			return executeWriteFileTool(ctx, conn, options.Clock, args, options.ResolvePlanPath)
		},
	)
}

func executeWriteFileTool(
	ctx context.Context,
	conn workspacesdk.AgentConn,
	clock quartz.Clock,
	args WriteFileArgs,
	resolvePlanPath func(context.Context) (chatPath string, home string, err error),
) (fantasy.ToolResponse, error) {
	requestedPath := strings.TrimSpace(args.Path)
	if requestedPath == "" {
		return fantasy.NewTextErrorResponse("path is required"), nil
	}

	hasPlanFileName := looksLikePlanFileName(requestedPath)
	if hasPlanFileName && !isAbsolutePath(requestedPath) {
		return fantasy.NewTextErrorResponse(
			"plan files must use absolute paths; use the chat-specific absolute plan path",
		), nil
	}

	if resolvePlanPath != nil && hasPlanFileName {
		chatPath, home, err := resolvePlanPath(ctx)
		if resp, rejected := rejectSharedPlanPath(requestedPath, home, chatPath, err); rejected {
			return resp, nil
		}
	}

	err := AwaitAgentAnswer(withToolCallHeaders(ctx), clock, func(ctx context.Context) error {
		return conn.WriteFile(ctx, requestedPath, strings.NewReader(args.Content))
	})
	if result, ok := fileRequestErrorResult(ctx, WriteFileToolName, err); ok {
		return result, nil
	}
	return writeFileResult(err), nil
}

// writeFileResult converts an answer the agent gave to a write_file
// request, live or recorded, into the tool result.
func writeFileResult(err error) fantasy.ToolResponse {
	if err != nil {
		return fantasy.NewTextErrorResponse(agentAPIErrorMessage(err))
	}
	return toolResponse(map[string]any{"ok": true})
}
