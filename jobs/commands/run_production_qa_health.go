package commands

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/deployment-io/deployment-runner-kit/tasks"
)

// A production health check GETs a web service's HealthURL as its load
// balancer's target group does: healthy is a status from 200 to 400 inclusive
// (the matcher "200-400" deploy_aws_web_service.go sets). A redirect is not
// followed — a 3xx is the answer — and at most productionQAHealthBodyLimit
// bytes of the body are read.
const (
	productionQAHealthTimeout   = 10 * time.Second
	productionQAHealthAttempts  = 3
	productionQAHealthBodyLimit = 64 << 10
	productionQAHealthMinStatus = 200
	productionQAHealthMaxStatus = 400
)

// productionQAHealthRetryWait is the wait between attempts. A var for tests.
var productionQAHealthRetryWait = 10 * time.Second

// productionQAHealthTransport sends the health checks. It is used directly,
// not through an http.Client, so a redirect is never looked at: a 3xx is the
// answer even when its Location doesn't parse. A var for tests.
var productionQAHealthTransport http.RoundTripper = &http.Transport{
	Proxy:               http.ProxyFromEnvironment,
	DisableKeepAlives:   true,
	TLSHandshakeTimeout: productionQAHealthTimeout,
}

// healthChecks checks every target with a HealthURL, in order, and returns
// one criterion per check. stopped is true when the stop signal fired: the
// checks not done by then are left out.
func (rp *RunProductionQA) healthChecks(targets []productionQATarget, logsWriter io.Writer) (criteria []qaCriterion, stopped bool) {
	for _, t := range targets {
		if t.HealthURL == "" {
			continue
		}
		c, stopped := rp.healthCheck(t, logsWriter)
		if stopped {
			return criteria, true
		}
		criteria = append(criteria, c)
	}
	return criteria, false
}

// healthCheck GETs the target's HealthURL up to productionQAHealthAttempts
// times, productionQAHealthRetryWait apart, until it answers healthy. It
// returns true, without a criterion to keep, when the stop signal fires.
func (rp *RunProductionQA) healthCheck(t productionQATarget, logsWriter io.Writer) (qaCriterion, bool) {
	path := "/"
	if u, err := url.Parse(t.HealthURL); err == nil {
		path = u.RequestURI()
	}
	c := qaCriterion{Criterion: fmt.Sprintf("%s: GET %s answers with a status from %d to %d",
		t.Repository, path, productionQAHealthMinStatus, productionQAHealthMaxStatus)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-rp.stopSignal:
			cancel()
		case <-ctx.Done():
		}
	}()
	isStopped := func() bool {
		select {
		case <-rp.stopSignal:
			return true
		default:
			return false
		}
	}
	var answer string
	for attempt := 1; ; attempt++ {
		if isStopped() {
			return c, true
		}
		status, err := getProductionQAHealth(ctx, t.HealthURL)
		if isStopped() {
			return c, true
		}
		healthy := err == nil && status >= productionQAHealthMinStatus && status <= productionQAHealthMaxStatus
		if err != nil {
			answer = "no answer"
			c.Reason = fmt.Sprintf("no answer (%s)", healthCheckError(err))
		} else {
			answer = strconv.Itoa(status)
			c.Reason = "answered " + answer
		}
		if healthy {
			c.State = tasks.QACriterionPassed
			break
		}
		c.State = tasks.QACriterionFailed
		if attempt >= productionQAHealthAttempts {
			break
		}
		timer := time.NewTimer(productionQAHealthRetryWait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return c, true
		}
	}
	io.WriteString(logsWriter, fmt.Sprintf("Production QA: health check of %s GET %s → %s\n", t.Repository, t.HealthURL, answer))
	return c, false
}

// getProductionQAHealth GETs raw, within productionQAHealthTimeout and until
// ctx (cancelled by the stop signal) is done, and returns its status.
func getProductionQAHealth(ctx context.Context, raw string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, productionQAHealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return 0, err
	}
	resp, err := productionQAHealthTransport.RoundTrip(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, productionQAHealthBodyLimit))
	return resp.StatusCode, nil
}

// healthCheckError is a short description of a failed GET that names neither
// the URL nor the address: one of a fixed set, never the error's own text.
func healthCheckError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var hostErr x509.HostnameError
	var opErr *net.OpError
	var errno syscall.Errno
	var recordErr tls.RecordHeaderError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		return "timed out"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.As(err, &dnsErr):
		if dnsErr.IsNotFound {
			return "host not found"
		}
		return "DNS lookup failed"
	case errors.As(err, &certErr), errors.As(err, &hostErr):
		return "TLS certificate not valid"
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed"
	case errors.As(err, &errno):
		return errno.Error()
	case errors.As(err, &recordErr):
		return "not a TLS server"
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return "connection failed"
	}
	// Anything else may carry server-supplied text or an address: name only
	// that the request failed.
	return "request failed"
}

// healthOnlyRound is the round of a production check with no target to open
// in a browser: the health checks, then every acceptance criterion not
// checked. NoCriteria is true only when the round has no criterion at all: no
// acceptance criteria and no health check.
func healthOnlyRound(parameters map[string]interface{}, targets []productionQATarget, health []qaCriterion) qaRoundOutput {
	acceptance := readAcceptanceCriteria(parameters)
	var services []string
	for _, t := range targets {
		services = append(services, t.Repository+"'s")
	}
	reason := fmt.Sprintf("Not checked: no https address to open it in a browser — add a custom domain to %s service.",
		strings.Join(services, " or "))
	criteria := append([]qaCriterion(nil), health...)
	for _, a := range acceptance {
		criteria = append(criteria, qaCriterion{Criterion: a, State: tasks.QACriterionNotChecked, Reason: reason})
	}
	return qaRoundOutput{
		Round:      1,
		Status:     tasks.QAStatusCompleted,
		NoCriteria: len(acceptance) == 0 && len(health) == 0,
		Criteria:   criteria,
	}
}
