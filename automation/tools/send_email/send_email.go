package send_email

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ankit-arora/langchaingo/callbacks"
	"github.com/ankit-arora/langchaingo/tools"
	email2 "github.com/deployment-io/deployment-runner-kit/dependencies/email"
	"github.com/deployment-io/deployment-runner-kit/enums/email_providers"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/go-playground/validator/v10"
	"io"
)

func NewSmtpEmailImplementation(host, port, username, password string) (email2.EmailI, error) {
	smtpImpl, err := email2.GetProvider(email_providers.Smtp, &email2.Options{
		SmtpHost:     &host,
		SmtpPort:     &port,
		SmtpUsername: &username,
		SmtpPassword: &password,
	})
	if err != nil {
		return nil, fmt.Errorf("error getting Smtp implementation: %s", err)
	}
	return smtpImpl, nil
}

func NewTool(params map[string]interface{}, logsWriter io.Writer, callbacksHandler callbacks.Handler,
	debugOpenAICalls bool) (*Tool, error) {
	smtpHost, err := jobs.GetParameterValue[string](params, parameters_enums.SmtpHost)
	if err != nil {
		return nil, fmt.Errorf("error getting smtp host: %s", err)
	}
	smtpPort, err := jobs.GetParameterValue[string](params, parameters_enums.SmtpPort)
	if err != nil {
		return nil, fmt.Errorf("error getting smtp port: %s", err)
	}
	smtpUsername, err := jobs.GetParameterValue[string](params, parameters_enums.SmtpUsername)
	if err != nil {
		return nil, fmt.Errorf("error getting smtp username: %s", err)
	}
	smtpPassword, err := jobs.GetParameterValue[string](params, parameters_enums.SmtpPassword)
	if err != nil {
		return nil, fmt.Errorf("error getting smtp password: %s", err)
	}
	fromAddress, err := jobs.GetParameterValue[string](params, parameters_enums.EmailToolFromAddress)
	if err != nil {
		return nil, fmt.Errorf("error getting smtp from address: %s", err)
	}
	smtpImpl, err := NewSmtpEmailImplementation(smtpHost, smtpPort, smtpUsername, smtpPassword)
	if err != nil {
		return nil, fmt.Errorf("error getting smtp implementation: %s", err)
	}
	return &Tool{
		Params:           params,
		LogsWriter:       logsWriter,
		CallbacksHandler: callbacksHandler,
		SmtpImpl:         smtpImpl,
		FromAddress:      fromAddress,
		DebugOpenAICalls: debugOpenAICalls,
	}, nil
}

type Tool struct {
	Params           map[string]interface{}
	LogsWriter       io.Writer
	CallbacksHandler callbacks.Handler
	SmtpImpl         email2.EmailI
	FromAddress      string
	DebugOpenAICalls bool
}

type Input struct {
	EmailAddress     string `json:"email_address" validate:"required"`
	Subject          string `json:"subject" validate:"required"`
	HtmlMessage      string `json:"html_message" validate:"required"`
	PlainTextMessage string `json:"plain_text_message" validate:"required"`
}

func parseAndValidateInput(response string, input *Input) error {
	// Parse the JSON response into a map
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(response), &raw); err != nil {
		return fmt.Errorf("failed to parse JSON: %w", err)
	}
	emailAddress, ok := raw["email_address"]
	if !ok {
		return fmt.Errorf("email_address key not found")
	}
	subject, ok := raw["subject"]
	if !ok {
		return fmt.Errorf("subject key not found")
	}
	htmlMessage, ok := raw["html_message"]
	if !ok {
		return fmt.Errorf("html message key not found")
	}
	plainTextMessage, ok := raw["plain_text_message"]
	if !ok {
		return fmt.Errorf("plain text message key not found")
	}
	e, ok := emailAddress.(string)
	if !ok {
		return fmt.Errorf("email address is not a string")
	}
	//TODO add validation for checking valid email address
	s, ok := subject.(string)
	if !ok {
		return fmt.Errorf("subject is not a string")
	}
	hm, ok := htmlMessage.(string)
	if !ok {
		return fmt.Errorf("html message is not a string")
	}
	ptm, ok := plainTextMessage.(string)
	if !ok {
		return fmt.Errorf("plain text message is not a string")
	}
	// Map the parsed response into the struct
	input.EmailAddress = e
	input.Subject = s
	input.HtmlMessage = hm
	input.PlainTextMessage = ptm
	// Validate the struct
	validate := validator.New()
	if err := validate.Struct(input); err != nil {
		return fmt.Errorf("validation error: %w", err)
	}
	return nil
}

func (t *Tool) Name() string {
	return "sendEmail"
}

func (t *Tool) Description() string {
	description := `Sends an email to a specified recipient using SMTP configurations. 
The tool requires the following inputs: email_address (the recipient's email), subject (email subject), html_message (HTML-formatted content), and plain_text_message (plain-text fallback content). 
If the email_address is not provided, you should prompt the user to supply it. This tool ensures the email is delivered in both HTML and plain-text formats while maintaining configurability and customization via SMTP.
`
	return description
}

func (t *Tool) Parameters() map[string]any {
	parameters := map[string]any{
		"properties": map[string]any{
			"email_address":      map[string]string{"title": "email_address", "type": "string", "description": "The recipient’s email address."},
			"subject":            map[string]string{"title": "subject", "type": "string", "description": "The subject line of the email."},
			"html_message":       map[string]string{"title": "html_message", "type": "string", "description": "The content of the email to be sent in HTML format."},
			"plain_text_message": map[string]string{"title": "plain_text_message", "type": "string", "description": "The content of the email to be sent in plain text format."},
		},
		"required": []string{"email_address", "subject", "html_message", "plain_text_message"},
		"type":     "object",
	}
	return parameters
}

func (t *Tool) Call(ctx context.Context, input string) (string, error) {
	if len(input) == 0 {
		return "Input cannot be empty. Please provide a valid email address, subject, html message, and plain text message.", nil
	}
	if t.CallbacksHandler != nil {
		info := fmt.Sprintf("Sending email with input: %s", input)
		if !t.DebugOpenAICalls {
			if len(info) > 200 {
				info = info[:200] + "..."
			}
		}
		t.CallbacksHandler.HandleToolStart(ctx, info)
	}
	inputJsonObj := &Input{}
	jsonParseErr := parseAndValidateInput(input, inputJsonObj)
	if jsonParseErr != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error parsing input: %s", jsonParseErr))
		}
		return fmt.Sprintf("Email was not sent due to the following reason: %s", jsonParseErr), nil
	}
	fromAddress := email2.Address{
		Email: t.FromAddress,
	}
	tos := []*email2.Address{{Email: inputJsonObj.EmailAddress}}
	err := t.SmtpImpl.SendEmail(&fromAddress, tos, nil,
		&inputJsonObj.Subject, &inputJsonObj.HtmlMessage, &inputJsonObj.PlainTextMessage)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error sending email: %s", err))
		}
		return fmt.Sprintf("Email was not sent due to the following reason: %s", err), nil
	}
	out := fmt.Sprintf("Email sent successfuly to: %s", inputJsonObj.EmailAddress)

	if t.CallbacksHandler != nil {
		info := fmt.Sprintf("Exiting send email tool with output: %s", out)
		if !t.DebugOpenAICalls {
			if len(info) > 200 {
				info = info[:200] + "..."
			}
		}
		t.CallbacksHandler.HandleToolEnd(ctx, info)
	}
	return out, nil
}

var _ tools.ToolWithParameters = &Tool{}
