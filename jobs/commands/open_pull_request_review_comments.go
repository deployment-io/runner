package commands

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/oauth"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// reviewCommentsMax caps the inline comments of one review. Past a few dozen,
// comments bury the diff they annotate; the description has the full list.
const reviewCommentsMax = 30

// reviewCommentsBody is the review's own text. It does not say "the full review
// is in the description": the description's Review section is capped and sends
// its overflow to the job log. The count is the provider's placeholder for the
// number of comments actually posted: the server drops comments that are not
// on the diff after this body is written.
const reviewCommentsBody = "deployment.io review: " + oauth.PullRequestReviewPostedPlaceholder + " finding(s) on the changed lines below. Findings that could not be placed on a line are in the pull request description, or in the Step's job log when the description's review section is full."

// postReviewComments posts the latest completed review round's open findings
// for ONE repository as inline comments on its pull request, as a single
// review.
//
// NEVER FAILS THE STEP OR THE PULL REQUEST. The pull request exists and its
// description is the complete record; these comments are a view onto it. An
// RPC error, a provider that cannot post inline reviews, or an older
// deployment-server without the method is logged and the Job carries on.
func (opr *taskOpenPR) postReviewComments(idx int, entry tasks.RepositoryEntry, prNumber int) {
	plan, ok := opr.reviewComments(idx, entry)
	if !ok {
		return
	}
	if len(plan.comments) == 0 {
		if plan.unplaced > 0 {
			io.WriteString(opr.logsWriter, fmt.Sprintf("No inline review comments for repo %s: %d finding(s) had no line to attach to\n", entry.Name, plan.unplaced))
		}
		return
	}
	dto, err := opr.rpc().PostPullRequestReview(opr.ctx.OrganizationID, oauth.PostPullRequestReviewArgsV1{
		InstallationID: entry.InstallationID,
		RepoName:       entry.Name,
		PRNumber:       prNumber,
		Body:           reviewCommentsBody,
		Comments:       plan.comments,
	})
	if err != nil {
		io.WriteString(opr.logsWriter, fmt.Sprintf("Inline review comments not posted: %s (repo %s, PR #%d)\n", err, entry.Name, prNumber))
		return
	}
	if dto.Unsupported {
		io.WriteString(opr.logsWriter, fmt.Sprintf("Inline review comments not posted: the git provider does not support them (repo %s, PR #%d)\n", entry.Name, prNumber))
		return
	}
	line := fmt.Sprintf("Posted %d inline review comment(s); %d were not on the diff; %d had no line to attach to (repo %s, PR #%d)",
		dto.Posted, dto.Dropped, plan.unplaced, entry.Name, prNumber)
	if plan.capped > 0 {
		line += fmt.Sprintf("; %d more were over the %d-comment limit", plan.capped, reviewCommentsMax)
	}
	io.WriteString(opr.logsWriter, line+"\n")
}

// reviewCommentPlan is what one repository's pull request gets: the comments,
// how many findings had no line in it to attach to, and how many were left
// out over reviewCommentsMax.
type reviewCommentPlan struct {
	comments []oauth.PullRequestReviewCommentV1
	unplaced int
	capped   int
}

// placedFinding is a finding with the comment that places it on a line.
type placedFinding struct {
	finding reviewFindingOutput
	comment oauth.PullRequestReviewCommentV1
}

// reviewComments builds the inline comments for the repository at idx,
// highest severity first.
//
// ok is false when nothing may be posted at all: no review, participation Off,
// no completed round, or a latest completed round that did not review the
// tree being committed (a fix run's work was kept after it), whose locations
// would describe code that has since changed.
func (opr *taskOpenPR) reviewComments(idx int, entry tasks.RepositoryEntry) (reviewCommentPlan, bool) {
	var plan reviewCommentPlan
	review := opr.review
	if review == nil || review.Participation == "off" || review.Participation == "" || !review.FinalTreeReviewed {
		return plan, false
	}
	latest := latestCompletedRound(review)
	if latest == nil {
		return plan, false
	}
	// A finding fixed during review never gets a comment: its location
	// describes code the fix changed. The latest round's findings are the open
	// ones; the key check is belt and braces.
	fixed := map[string]bool{}
	for _, f := range review.FixedInLoop {
		fixed[findingKey(f)] = true
	}
	repoDir := repoDirRelativeToWorkDir(
		commandUtils.GetTaskRepositoryDir(opr.ctx.OrganizationID, opr.ctx.TaskID, idx, entry.Name),
		commandUtils.GetTaskRepositoriesBaseDir(opr.ctx.OrganizationID, opr.ctx.TaskID))
	var candidates []placedFinding
	for _, f := range latest.Findings {
		if fixed[findingKey(f)] {
			continue
		}
		comment, placement := placeFinding(f, repoDir, len(opr.ctx.Entries) == 1)
		switch placement {
		case findingPlaced:
			candidates = append(candidates, placedFinding{finding: f, comment: comment})
		case findingUnplaced:
			plan.unplaced++
		}
	}
	sortBySeverity(candidates)
	if len(candidates) > reviewCommentsMax {
		plan.capped = len(candidates) - reviewCommentsMax
		candidates = candidates[:reviewCommentsMax]
	}
	for _, c := range candidates {
		plan.comments = append(plan.comments, c.comment)
	}
	return plan, true
}

type findingPlacement int

const (
	findingPlaced findingPlacement = iota
	// findingUnplaced: no line in this repository to attach to.
	findingUnplaced
	// findingOtherRepository: another repository's pull request's business.
	findingOtherRepository
)

// placeFinding turns a finding's location into a comment on this repository's
// pull request. A reviewer does not always write the repository prefix: with
// one repository (onlyRepo) an unprefixed path can only be that repository's;
// with several there is no telling which one it names.
func placeFinding(f reviewFindingOutput, repoDir string, onlyRepo bool) (oauth.PullRequestReviewCommentV1, findingPlacement) {
	var c oauth.PullRequestReviewCommentV1
	path, start, end, parsed := parseFindingLocation(f.Location)
	if !parsed {
		return c, findingUnplaced
	}
	relative, mine, prefixed := repoRelativePath(path, repoDir)
	switch {
	case mine:
	case prefixed:
		return c, findingOtherRepository
	case !onlyRepo:
		return c, findingUnplaced
	}
	c = oauth.PullRequestReviewCommentV1{Path: relative, Line: end, Body: reviewCommentBody(f)}
	if start < end {
		c.StartLine = start
	}
	return c, findingPlaced
}

// sortBySeverity puts the highest severity first and, within one severity,
// orders findings the way the description does: must-fix, then sent back and
// still open, then noted.
func sortBySeverity(candidates []placedFinding) {
	sort.SliceStable(candidates, func(i, j int) bool {
		si, sj := findingSeverity(candidates[i].finding), findingSeverity(candidates[j].finding)
		if si != sj {
			return si > sj
		}
		return findingClassRank(candidates[i].finding) < findingClassRank(candidates[j].finding)
	})
}

// repoRelativePath strips this repository's directory prefix from a finding's
// path. mine: the path is under repoDir. prefixed: the path starts with some
// "<idx>-" repository directory — this one or another.
func repoRelativePath(path, repoDir string) (relative string, mine, prefixed bool) {
	path = strings.TrimPrefix(path, "/work/")
	path = strings.TrimPrefix(path, "./")
	if rest, found := strings.CutPrefix(path, repoDir+"/"); found && rest != "" {
		return rest, true, true
	}
	return path, false, repoDirPrefix.MatchString(path)
}

// repoDirPrefix matches the "<idx>-" a Task's repository directory starts
// with (see commandUtils.GetTaskRepositoryDir).
var repoDirPrefix = regexp.MustCompile(`^\d+-[^/]+/[^/]+/`)

// findingLocationPattern parses "path:LINE", "path:LINE-END" and
// "path:LINE:COLUMN". The path is matched lazily so a column is never read as
// the line.
var findingLocationPattern = regexp.MustCompile(`^(.+?):(\d+)(?:-(\d+)|:(\d+))?$`)

// parseFindingLocation returns the path and the first and last line a
// finding's location names. A location with no line, or one that does not
// parse, is not placed.
func parseFindingLocation(location string) (path string, start, end int, ok bool) {
	location = strings.Trim(strings.TrimSpace(location), "`")
	m := findingLocationPattern.FindStringSubmatch(location)
	if m == nil {
		return "", 0, 0, false
	}
	path = strings.TrimSpace(m[1])
	start, err := strconv.Atoi(m[2])
	if err != nil || start <= 0 || path == "" {
		return "", 0, 0, false
	}
	end = start
	if m[3] != "" {
		// A range whose end does not parse or comes before its start is
		// malformed: leave it in the body rather than place its first line.
		e, err := strconv.Atoi(m[3])
		if err != nil || e < start {
			return "", 0, 0, false
		}
		end = e
	}
	return path, start, end, true
}

func findingSeverity(f reviewFindingOutput) uint {
	v, _ := tasks.ReviewSeverityValue(f.Severity)
	return v
}

// findingClassRank orders findings of equal severity the way the description
// does: must-fix, then sent back and still open, then noted.
func findingClassRank(f reviewFindingOutput) int {
	switch {
	case f.MustFix:
		return 0
	case f.SentBack:
		return 1
	}
	return 2
}

// reviewCommentBody renders one finding as a comment. Plain and bounded by the
// Review section's caps; every model-written field goes through
// neutraliseModelText.
func reviewCommentBody(f reviewFindingOutput) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**%s · %s** — %s",
		neutraliseModelText(capRunes(strings.TrimSpace(f.Parameter), reviewLocationMaxRunes)),
		neutraliseModelText(capRunes(strings.TrimSpace(f.Severity), reviewLocationMaxRunes)),
		findingClassLabel(f)))
	// Only a held finding went through a fix round and survived it. A
	// must-fix finding first reported in the final round never had one.
	if f.Held {
		sb.WriteString(", still present after a fix round")
	}
	sb.WriteString("\n")
	if what := strings.TrimSpace(f.What); what != "" {
		sb.WriteString("\n" + neutraliseModelText(capRunes(what, reviewDetailMaxRunes)) + "\n")
	}
	if why := strings.TrimSpace(f.Why); why != "" {
		sb.WriteString("\n_Why it matters:_ " + neutraliseModelText(capRunes(why, reviewDetailMaxRunes)) + "\n")
	}
	if note := strings.TrimSpace(f.StillPresentNote); f.Held && note != "" {
		sb.WriteString("\n_Reviewer's note:_ " + neutraliseModelText(capRunes(note, reviewDetailMaxRunes)) + "\n")
	}
	return sb.String()
}

// findingClassLabel is worded to be true on every path to the finding.
func findingClassLabel(f reviewFindingOutput) string {
	switch {
	case f.MustFix:
		return "must fix before merge"
	case f.SentBack:
		return "still open, does not hold this pull request"
	}
	return "noted"
}

// modelTextEscaper neutralises model output for a comment: a zero-width space
// after @ and # so a finding cannot ping a person or link an issue, and
// backslash escapes on link, image and autolink syntax so it cannot render a
// link. The backslash itself is escaped first, so "\[" cannot undo it. A
// zero-width space inside "://" and after "www" stops GitHub's extended
// autolinks from turning a bare URL into a link.
var modelTextEscaper = strings.NewReplacer(
	`\`, `\\`,
	"@", "@\u200b",
	"#", "#\u200b",
	"[", `\[`,
	"]", `\]`,
	"<", `\<`,
	">", `\>`,
	"://", ":\u200b//",
)

// bareWWWLink matches the "www." that starts a GitHub extended autolink, in
// any case.
var bareWWWLink = regexp.MustCompile(`(?i)\b(www)\.`)

func neutraliseModelText(s string) string {
	return bareWWWLink.ReplaceAllString(modelTextEscaper.Replace(s), "${1}\u200b.")
}
