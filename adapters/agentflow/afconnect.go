// Package afconnect exposes connector tools to an agentflow agent loop.
//
//	tools := afconnect.Specs(c)
//	catalog, _ := afagent.PrepareTools(ctx, declared, tools, allow)
//	out, err := catalog.Dispatch(ctx, call, declaredFn, afconnect.Dispatcher(c, creds))
//
// It lives in its own module so the connectors core does not depend on the
// model SDKs agentflow's adapters pull in.
package afconnect

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/omniaura/agentflow/runtime/afagent"
	"github.com/omniaura/agentflow/runtime/afhttp"
	"github.com/omniaura/connectors"
	"github.com/omniaura/go-kit/errs"
)

// Specs returns the connector's tools as agentflow tool specs.
func Specs(c *connectors.Connector) []afagent.ToolSpec {
	tools := c.Tools()
	out := make([]afagent.ToolSpec, 0, len(tools))
	for _, t := range tools {
		out = append(out, afagent.ToolSpec{Name: t.Name, Description: t.Description, Schema: t.Schema})
	}
	return out
}

// CredentialSource returns the credentials for the person the run acts for.
type CredentialSource func(context.Context) (connectors.Credentials, error)

// Dispatcher runs a tool call through the connector. App answers, including
// 4xx and 5xx, become the tool output the model sees. So do the connector's
// own 4xx errors (a bad or missing argument), rendered by afhttp.ToolMessage
// so the model can correct the call; anything else is returned as an error.
func Dispatcher(c *connectors.Connector, creds CredentialSource) func(context.Context, afagent.ToolCall) (string, error) {
	return func(ctx context.Context, call afagent.ToolCall) (string, error) {
		values, err := creds(ctx)
		if err != nil {
			return "", err
		}
		r, err := c.Execute(ctx, values, call.Name, json.RawMessage(call.Arguments))
		if err != nil {
			var e *errs.Error
			if errors.As(err, &e) && connectors.ErrInvalidArguments.Is(e) {
				return afhttp.ToolMessage(e), nil
			}
			return "", err
		}
		return r.String(), nil
	}
}
