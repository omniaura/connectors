package connectors

import (
	"net/http"

	"github.com/omniaura/go-kit/errs"
)

// Errors follow the go-kit errs contract: the message is safe to show a
// person; the cause and upstream detail are server-only.
var (
	// ErrCredentialsRejected means the app answered the credential check
	// with 401 or 403.
	ErrCredentialsRejected = errs.NewFactory(http.StatusUnprocessableEntity,
		"The app rejected these credentials. Check the key and try again.",
		errs.WithCode("connector_credentials_rejected"), errs.WithAction(errs.ActionUpdateSettings))
	// ErrMissingCredentials means a credential field the app needs is empty.
	ErrMissingCredentials = errs.NewFactory(http.StatusUnprocessableEntity,
		"This app needs credentials before it can be used.",
		errs.WithCode("connector_credentials_missing"), errs.WithAction(errs.ActionUpdateSettings))
	// ErrCheckUnavailable means the credential check could not reach a
	// verdict: a network failure, a 5xx, or an unexpected status.
	ErrCheckUnavailable = errs.NewFactory(http.StatusBadGateway,
		"Couldn't check these credentials with the app right now. Try again in a moment.",
		errs.WithCode("connector_check_unavailable"), errs.WithAction(errs.ActionRetry))
	// ErrNoCheck means the manifest declares no credential check.
	ErrNoCheck = errs.NewFactory(http.StatusNotImplemented,
		"This app has no way to check credentials before they are saved.",
		errs.WithCode("connector_no_check"))
	// ErrUnknownTool means the tool name is not one this connector offers.
	ErrUnknownTool = errs.NewFactory(http.StatusNotFound,
		"That tool isn't available for this app.",
		errs.WithCode("connector_unknown_tool"))
	// ErrInvalidArguments means the call's arguments do not fit the tool.
	ErrInvalidArguments = errs.NewFactory(http.StatusBadRequest,
		"The tool call's arguments don't match what the tool accepts.",
		errs.WithCode("connector_invalid_arguments"), errs.WithAction(errs.ActionFixInput))
	// ErrRequestFailed means the request never got an HTTP answer.
	ErrRequestFailed = errs.NewFactory(http.StatusBadGateway,
		"Couldn't reach the app. Try again in a moment.",
		errs.WithCode("connector_request_failed"), errs.WithAction(errs.ActionRetry))
	// ErrSpecUnavailable means the OpenAPI document could not be loaded.
	ErrSpecUnavailable = errs.NewFactory(http.StatusBadGateway,
		"Couldn't load this app's API description.",
		errs.WithCode("connector_spec_unavailable"))
)
