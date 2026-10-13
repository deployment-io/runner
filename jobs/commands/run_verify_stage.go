package commands

import "io"

// RunVerifyStage is the QA stage: between RunReviewStage and CommitAndPush, a
// separate agent run deploys the Task's static-site previews and checks the
// spec's acceptance criteria in a real browser.
//
// A PLACEHOLDER for now. The control plane does not list the command in a
// Step Job yet; it will once every runner knows it, because a runner fails a
// Job on a command it does not know. Until the stage is implemented a runner
// that receives it passes the parameters through and writes nothing.
type RunVerifyStage struct{}

func (rv *RunVerifyStage) Run(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error) {
	return parameters, nil
}
