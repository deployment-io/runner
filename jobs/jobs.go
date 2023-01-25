package jobs

type Runner interface {
	Run() error
}

type Job interface {
	Runner
}
