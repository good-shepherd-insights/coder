package chattool

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/codersdk/workspacesdk"
	"github.com/coder/quartz"
)

// ToolCallAge measures time since a tool call was committed without
// comparing coderd's clock with the database's. The age at the
// database read comes from database timestamps only, and time since
// that read comes from coderd's monotonic clock.
type ToolCallAge struct {
	atRead time.Duration
	readAt time.Time
	clock  quartz.Clock
}

// NewToolCallAge returns the age of a tool call committed at
// committedAt, where dbNow is the database clock read just before this
// call and committedAt is the assistant message's database timestamp.
func NewToolCallAge(clock quartz.Clock, dbNow, committedAt time.Time) ToolCallAge {
	return ToolCallAge{
		atRead: dbNow.Sub(committedAt),
		readAt: clock.Now(),
		clock:  clock,
	}
}

// Now returns the current tool call age. It is never negative. The
// zero value reports zero.
func (a ToolCallAge) Now() time.Duration {
	if a.clock == nil {
		return 0
	}
	return max(a.atRead+a.clock.Since(a.readAt), 0)
}

// ToolCallIdentity identifies the committed tool call a tool is running
// for: the chat, the assistant message that contains the call, and the
// provider tool call ID.
type ToolCallIdentity struct {
	ChatID     uuid.UUID
	MessageID  int64
	ToolCallID string
	Age        ToolCallAge
}

// AgentToolCall returns the tool call to attach to a workspace agent
// request with workspacesdk.WithToolCall. Its age is measured now, so
// build it immediately before the request.
func (id ToolCallIdentity) AgentToolCall() workspacesdk.ToolCall {
	return workspacesdk.ToolCall{
		MessageID: id.MessageID,
		ID:        id.ToolCallID,
		Age:       id.Age.Now(),
	}
}

// UUID returns the tool call UUID. The workspace agent uses it as the ID
// of a process started for this tool call; an agent without tool call
// support picks another.
func (id ToolCallIdentity) UUID() string {
	return workspacesdk.ToolCallUUID(id.ChatID, id.MessageID, id.ToolCallID).String()
}

type toolCallIdentityKey struct{}

// WithToolCallIdentity returns a context carrying id for the tool call
// run with it.
func WithToolCallIdentity(ctx context.Context, id ToolCallIdentity) context.Context {
	return context.WithValue(ctx, toolCallIdentityKey{}, id)
}

// ToolCallIdentityFromContext returns the identity set by
// WithToolCallIdentity. ok is false when the tool call has none, for
// example outside a chat generation.
func ToolCallIdentityFromContext(ctx context.Context) (ToolCallIdentity, bool) {
	id, ok := ctx.Value(toolCallIdentityKey{}).(ToolCallIdentity)
	return id, ok
}

// AgentErrorKind classifies an error from a workspace agent request made
// for a tool call.
type AgentErrorKind int

const (
	// AgentErrorRefused is a *workspacesdk.ToolCallError: the agent
	// refused the tool call and did nothing.
	AgentErrorRefused AgentErrorKind = iota + 1
	// AgentErrorResponse is any other HTTP error answer from the agent
	// (*codersdk.Error).
	AgentErrorResponse
	// AgentErrorUnreachable means no answer arrived. The request may
	// still have reached the agent.
	AgentErrorUnreachable
	// AgentErrorUnreadable means the agent answered, but the answer
	// could not be read, so it may have acted.
	AgentErrorUnreadable
	// AgentErrorNoAnswer means the agent did not answer within
	// AgentAnswerTimeout (ErrAgentAnswerTimeout). The request may have
	// arrived.
	AgentErrorNoAnswer
)

// ClassifyAgentError classifies err, returned by a workspace agent
// request made for a tool call. code is set for AgentErrorRefused.
func ClassifyAgentError(err error) (kind AgentErrorKind, code workspacesdk.ToolCallErrorCode) {
	if errors.Is(err, ErrAgentAnswerTimeout) {
		return AgentErrorNoAnswer, ""
	}
	var tcErr *workspacesdk.ToolCallError
	if errors.As(err, &tcErr) {
		return AgentErrorRefused, tcErr.Code
	}
	var sdkErr *codersdk.Error
	if errors.As(err, &sdkErr) {
		return AgentErrorResponse, ""
	}
	// http.Client.Do returns every transport failure as a *url.Error.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return AgentErrorUnreachable, ""
	}
	return AgentErrorUnreadable, ""
}

// AgentAnswerTimeout is how long chatd waits for the workspace agent to
// answer one start, edit, write, or cancel request made for a tool call.
// It bounds the wait for the answer, never the work, so every attempt
// ends with an answer or an unknown outcome even when a connected agent
// never answers.
const AgentAnswerTimeout = time.Minute

// ErrAgentAnswerTimeout is wrapped by the error of a request that
// AwaitAgentAnswer ended because the agent did not answer in time.
var ErrAgentAnswerTimeout = xerrors.New("the workspace agent did not answer within " + AgentAnswerTimeout.String())

// AwaitAgentAnswer calls request with a context that also ends after
// AgentAnswerTimeout on clock, and returns its error. When that timeout
// ended the request, the error wraps ErrAgentAnswerTimeout; when ctx
// ended it, the error is returned unchanged. A nil error is returned
// unchanged even if the timeout fired meanwhile, because the answer was
// received. A nil clock means a real clock.
func AwaitAgentAnswer(ctx context.Context, clock quartz.Clock, request func(ctx context.Context) error) error {
	if clock == nil {
		clock = quartz.NewReal()
	}
	reqCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := clock.AfterFunc(AgentAnswerTimeout, func() { cancel(ErrAgentAnswerTimeout) }, "chattool", "agent-answer")
	defer timer.Stop()
	err := request(reqCtx)
	if err != nil && errors.Is(context.Cause(reqCtx), ErrAgentAnswerTimeout) {
		return xerrors.Errorf("%v: %w", err, ErrAgentAnswerTimeout)
	}
	return err
}

// AgentNoAnswerReason is why the outcome of a tool call is unknown when
// the workspace agent did not answer within AgentAnswerTimeout.
var AgentNoAnswerReason = ErrAgentAnswerTimeout.Error()

// AgentRestartedReason is why the outcome of a tool call refused with
// workspacesdk.ToolCallErrorAgentStartedAfterToolCall is unknown.
const AgentRestartedReason = "the workspace agent restarted after this tool call"

// AgentUnreachableReason is why the outcome of a tool call is unknown
// when the workspace agent could not be reached.
func AgentUnreachableReason(err error) string {
	return fmt.Sprintf("the workspace agent could not be reached (%v)", err)
}

// AgentUnreadableReason is why the outcome of a tool call is unknown
// when the workspace agent's answer could not be read.
func AgentUnreadableReason(err error) string {
	return fmt.Sprintf("the workspace agent's answer could not be read (%v)", err)
}

// UnknownOutcome words a tool call result whose side effect may or may
// not have happened: reason is one of the reasons above, effect says
// what may have happened, and check how the model can find out.
func UnknownOutcome(reason, effect, check string) string {
	return fmt.Sprintf("outcome unknown: %s, so %s. %s", reason, effect, check)
}

// AgentErrorWords are the words a tool uses in the results AgentErrorText
// builds.
type AgentErrorWords struct {
	// Action names the request, as in "start process".
	Action string
	// Existing names what the agent recorded for the tool call, as in
	// "a process for this tool call".
	Existing string
	// Effect says what may have happened when the outcome is unknown, as
	// in "the command may have started".
	Effect string
	// Check tells the model how to find out, as in "Check it with
	// process_output using process ID <UUID>."
	Check string
	// RestartedEffect and RestartedCheck replace Effect and Check after
	// an agent_started_after_tool_call refusal. Empty means Effect or
	// Check.
	RestartedEffect, RestartedCheck string
}

// AgentErrorText returns the result text for err, returned by a workspace
// agent request made for a tool call. ok is false when the tool reports
// err as it does without a tool call identity: an HTTP error answer, or
// a stale_tool_call or tool_call_canceled refusal, which only reaches an
// attempt whose result is not committed.
func AgentErrorText(err error, words AgentErrorWords) (text string, ok bool) {
	kind, code := ClassifyAgentError(err)
	switch {
	case kind == AgentErrorRefused && code == workspacesdk.ToolCallErrorAgentStartedAfterToolCall:
		return UnknownOutcome(AgentRestartedReason,
			cmp.Or(words.RestartedEffect, words.Effect), cmp.Or(words.RestartedCheck, words.Check)), true
	case kind == AgentErrorRefused && code == workspacesdk.ToolCallErrorInputMismatch:
		return fmt.Sprintf("%s: this request changed nothing because %s already exists with a different input: %v",
			words.Action, words.Existing, err), true
	case kind == AgentErrorUnreachable:
		return UnknownOutcome(AgentUnreachableReason(err), words.Effect, words.Check), true
	case kind == AgentErrorUnreadable:
		return UnknownOutcome(AgentUnreadableReason(err), words.Effect, words.Check), true
	case kind == AgentErrorNoAnswer:
		return UnknownOutcome(AgentNoAnswerReason, words.Effect, words.Check), true
	default:
		return "", false
	}
}
