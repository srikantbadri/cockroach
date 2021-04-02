// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package issues

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"text/template"

	"github.com/cockroachdb/cockroach/pkg/util/version"
	"github.com/cockroachdb/errors"
	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

const (
	// CockroachPkgPrefix is the crdb package prefix.
	CockroachPkgPrefix = "github.com/cockroachdb/cockroach/pkg/"
	// Based on the following observed API response the maximum here is 1<<16-1.
	// We shouldn't usually get near that limit but if we do, better to post a
	// clipped issue.
	//
	// 422 Validation Failed [{Resource:Issue Field:body Code:custom Message:body
	// is too long (maximum is 65536 characters)}]
	githubIssueBodyMaximumLength = 60000
)

func enforceMaxLength(s string) string {
	if len(s) > githubIssueBodyMaximumLength {
		return s[:githubIssueBodyMaximumLength]
	}
	return s
}

// UnitTestFailureTitle is a title template suitable for posting issues about
// vanilla Go test failures.
const UnitTestFailureTitle = `{{ shortpkg .PackageName }}: {{.TestName}} failed`

// UnitTestFailureBody is a body template suitable for posting issues about vanilla Go
// test failures.
const UnitTestFailureBody = `[({{shortpkg .PackageName}}).{{.TestName}} failed]({{.URL}}) on [{{.Branch}}@{{.Commit}}]({{commiturl .Commit}}):

{{ if (.CondensedMessage.FatalOrPanic 50).Error }}{{with $fop := .CondensedMessage.FatalOrPanic 50 -}}
Fatal error:
{{threeticks}}
{{ .Error }}{{threeticks}}

Stack:
{{threeticks}}
{{ $fop.FirstStack }}
{{threeticks}}

<details><summary>Log preceding fatal error</summary><p>

{{threeticks}}
{{ $fop.LastLines }}
{{threeticks}}

</p></details>{{end}}{{ else -}}
{{threeticks}}
{{ .CondensedMessage.Digest 50 }}
{{ threeticks }}{{end}}

<details><summary>More</summary><p>
{{if .Parameters -}}
Parameters:
{{range .Parameters }}
- {{ . }}{{end}}{{end}}

{{if .ArtifactsURL }}Artifacts: [{{.Artifacts}}]({{ .ArtifactsURL }})
{{else -}}
{{threeticks}}
{{.ReproductionCommand}}
{{threeticks}}

{{end -}}

{{ if .RelatedIssues }}Related:{{end}}{{range .RelatedIssues}}
- #{{ .Number}} {{ .Title }} {{ range .Labels }} [{{ .Name }}]({{ .URL }}){{- end}}
{{end}}
[See this test on roachdash](https://roachdash.crdb.dev/?filter={{urlquery "status:open t:.*" .TestName ".*" }}&sort=title&restgroup=false&display=lastcommented+project)
<sub>powered by [pkg/cmd/internal/issues](https://github.com/cockroachdb/cockroach/tree/master/pkg/cmd/internal/issues)</sub></p></details>
`

var (
	// Set of labels attached to created issues.
	issueLabels = []string{"O-robot", "C-test-failure"}
	// Label we expect when checking existing issues. Sometimes users open
	// issues about flakes and don't assign all the labels. We want to at
	// least require the test-failure label to avoid pathological situations
	// in which a test name is so generic that it matches lots of random issues.
	// Note that we'll only post a comment into an existing label if the labels
	// match 100%, but we also cross-link issues whose labels differ. But we
	// require that they all have searchLabel as a baseline.
	searchLabel = issueLabels[1]
)

// If the assignee would be the key in this map, assign to the value instead.
// Helpful to avoid pinging former employees.
// An "" value means that issues that would have gone to the key are left
// unassigned.
var oldFriendsMap = map[string]string{
	"a-robinson":   "andreimatei",
	"benesch":      "nvanbenschoten",
	"georgeutsin":  "yuzefovich",
	"tamird":       "tbg",
	"rohany":       "solongordon",
	"vivekmenezes": "",
	"lucy-zhang":   "ajwerner",
}

// context augments context.Context with a logger.
type postCtx struct {
	context.Context
	strings.Builder
}

func (ctx *postCtx) Printf(format string, args ...interface{}) {
	if n := len(format); n > 0 && format[n-1] != '\n' {
		format += "\n"
	}
	fmt.Fprintf(&ctx.Builder, format, args...)
}

func (p *poster) getAssignee(ctx *postCtx, authorEmail string) string {
	if authorEmail == "" {
		ctx.Printf("no author provided")
		return ""
	}
	commits, _, err := p.listCommits(ctx, p.Org, p.Repo, &github.CommitsListOptions{
		Author: authorEmail,
		ListOptions: github.ListOptions{
			PerPage: 1,
		},
	})
	if err != nil {
		ctx.Printf("unable list commits by %s: %v", authorEmail, err)
		return ""
	}
	if len(commits) == 0 {
		ctx.Printf("no GitHub commits found for email %s", authorEmail)
		return ""
	}

	if commits[0].Author == nil {
		ctx.Printf("no Author found for user email %s", authorEmail)
		return ""
	}
	assignee := *commits[0].Author.Login

	if newAssignee, ok := oldFriendsMap[assignee]; ok {
		if newAssignee == "" {
			ctx.Printf("%s marked as alumn{us,a}; leaving issue unassigned", assignee)
			return ""
		}
		ctx.Printf("%s marked as alumn{us/a}; assigning to %s instead", assignee, newAssignee)
		return newAssignee
	}
	return assignee
}

func getLatestTag() (string, error) {
	cmd := exec.Command("git", "describe", "--abbrev=0", "--tags", "--match=v[0-9]*")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (p *poster) getProbableMilestone(ctx *postCtx) *int {
	tag, err := p.getLatestTag()
	if err != nil {
		ctx.Printf("unable to get latest tag to determine milestone: %s", err)
		return nil
	}

	v, err := version.Parse(tag)
	if err != nil {
		ctx.Printf("unable to parse version from tag to determine milestone: %s", err)
		return nil
	}
	vstring := fmt.Sprintf("%d.%d", v.Major(), v.Minor())

	milestones, _, err := p.listMilestones(ctx, p.Org, p.Repo, &github.MilestoneListOptions{
		State: "open",
	})
	if err != nil {
		ctx.Printf("unable to list milestones for %s/%s: %v", p.Org, p.Repo, err)
		return nil
	}

	for _, m := range milestones {
		if m.GetTitle() == vstring {
			return m.Number
		}
	}
	return nil
}

type poster struct {
	*Options

	createIssue func(ctx context.Context, owner string, repo string,
		issue *github.IssueRequest) (*github.Issue, *github.Response, error)
	searchIssues func(ctx context.Context, query string,
		opt *github.SearchOptions) (*github.IssuesSearchResult, *github.Response, error)
	createComment func(ctx context.Context, owner string, repo string, number int,
		comment *github.IssueComment) (*github.IssueComment, *github.Response, error)
	listCommits func(ctx context.Context, owner string, repo string,
		opts *github.CommitsListOptions) ([]*github.RepositoryCommit, *github.Response, error)
	listMilestones func(ctx context.Context, owner string, repo string,
		opt *github.MilestoneListOptions) ([]*github.Milestone, *github.Response, error)
	createProjectCard func(ctx context.Context, columnID int64,
		opt *github.ProjectCardOptions) (*github.ProjectCard, *github.Response, error)
}

func newPoster(client *github.Client, opts *Options) *poster {
	return &poster{
		Options:           opts,
		createIssue:       client.Issues.Create,
		searchIssues:      client.Search.Issues,
		createComment:     client.Issues.CreateComment,
		listCommits:       client.Repositories.ListCommits,
		listMilestones:    client.Issues.ListMilestones,
		createProjectCard: client.Projects.CreateProjectCard,
	}
}

// Options configures the issue poster.
type Options struct {
	Token        string // Github API token
	Org          string
	Repo         string
	SHA          string
	BuildID      string
	ServerURL    string
	Branch       string
	Tags         string
	Goflags      string
	getLatestTag func() (string, error)
}

// DefaultOptionsFromEnv initializes the Options from the environment variables,
// falling back to placeholders if the environment is not or only partially
// populated.
func DefaultOptionsFromEnv() *Options {
	// NB: these are hidden here as "proof" that nobody uses them directly
	// outside of this method.
	const (
		githubOrgEnv           = "GITHUB_ORG"
		githubRepoEnv          = "GITHUB_REPO"
		githubAPITokenEnv      = "GITHUB_API_TOKEN"
		teamcityVCSNumberEnv   = "BUILD_VCS_NUMBER"
		teamcityBuildIDEnv     = "TC_BUILD_ID"
		teamcityServerURLEnv   = "TC_SERVER_URL"
		teamcityBuildBranchEnv = "TC_BUILD_BRANCH"
		tagsEnv                = "TAGS"
		goFlagsEnv             = "GOFLAGS"
	)

	return &Options{
		Token: maybeEnv(githubAPITokenEnv, ""),
		Org:   maybeEnv(githubOrgEnv, "cockroachdb"),
		Repo:  maybeEnv(githubRepoEnv, "cockroach"),
		// The default value is the very first commit in the repository.
		// This was chosen simply because it exists and while surprising,
		// at least it'll be obvious that something went wrong (as an
		// issue will be posted pointing at that SHA).
		SHA:          maybeEnv(teamcityVCSNumberEnv, "8548987813ff9e1b8a9878023d3abfc6911c16db"),
		BuildID:      maybeEnv(teamcityBuildIDEnv, "NOTFOUNDINENV"),
		ServerURL:    maybeEnv(teamcityServerURLEnv, "https://server-url-not-found-in-env.com"),
		Branch:       maybeEnv(teamcityBuildBranchEnv, "branch-not-found-in-env"),
		Tags:         maybeEnv(tagsEnv, ""),
		Goflags:      maybeEnv(goFlagsEnv, ""),
		getLatestTag: getLatestTag,
	}
}

func maybeEnv(envKey, defaultValue string) string {
	v := os.Getenv(envKey)
	if v == "" {
		return defaultValue
	}
	return v
}

// CanPost returns true if the github API token environment variable is set to
// a nontrivial value.
func (o *Options) CanPost() bool {
	return o.Token != ""
}

// TemplateData holds the data available in (PostRequest).(Body|Title)Template,
// respectively. On top of the below, there are also a few functions, for which
// UnitTestFailureBody can serve as a reference.
type TemplateData struct {
	PostRequest
	Parameters       []string
	CondensedMessage CondensedMessage
	Commit           string
	Branch           string
	ArtifactsURL     string
	URL              string
	Assignee         interface{} // lazy
	RelatedIssues    []github.Issue
	InternalLog      string
}

func (p *poster) templateData(
	ctx context.Context, req PostRequest, assignee string, relatedIssues []github.Issue,
) TemplateData {
	var artifactsURL string
	if req.Artifacts != "" {
		artifactsURL = p.teamcityArtifactsURL(req.Artifacts).String()
	}
	return TemplateData{
		PostRequest:      req,
		Parameters:       p.parameters(),
		CondensedMessage: CondensedMessage(req.Message),
		Branch:           p.Branch,
		Commit:           p.SHA,
		ArtifactsURL:     artifactsURL,
		URL:              p.teamcityBuildLogURL().String(),
		Assignee:         assignee,
		RelatedIssues:    relatedIssues,
	}
}

func (p *poster) execTemplate(ctx context.Context, tpl string, data TemplateData) (string, error) {
	tlp, err := template.New("").Funcs(template.FuncMap{
		"threeticks": func() string { return "```" },
		"commiturl": func(sha string) string {
			return fmt.Sprintf("https://github.com/%s/%s/commits/%s", p.Org, p.Repo, p.SHA)
		},
		"shortpkg": func(fullpkg string) string {
			return strings.TrimPrefix(fullpkg, CockroachPkgPrefix)
		},
	}).Parse(tpl)
	if err != nil {
		return "", err
	}

	var buf strings.Builder
	if err := tlp.Execute(&buf, data); err != nil {
		return "", err
	}
	return enforceMaxLength(buf.String()), nil
}

type postBuilder struct {
	strings.Builder // the body
	p               *poster
	PostRequest
}

func (p *poster) post(
	origCtx context.Context,
	req PostRequest,
	titleFn func(io.Writer, TemplateData) error,
	bodyFn func(io.Writer, TemplateData) error,
) error {
	ctx := &postCtx{Context: origCtx}

	assignee := p.getAssignee(ctx, req.AuthorEmail)

	data := p.templateData(
		ctx,
		req,
		assignee,
		nil, // relatedIssues
	)

	if titleFn == nil {
		titleFn = func(w io.Writer, data TemplateData) error {
			title, err := p.execTemplate(ctx, req.TitleTemplate, data)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(w, title)
			return err
		}
	}
	if bodyFn == nil {
		bodyFn = func(w io.Writer, data TemplateData) error {
			body, err := p.execTemplate(ctx, req.BodyTemplate, data)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(w, body)
			return err
		}
	}

	// We just want the title this time around, as we're going to use
	// it to figure out if an issue already exists.
	title, err := func() (string, error) {
		var buf bytes.Buffer
		if err := titleFn(&buf, data); err != nil {
			return "", err
		}
		return buf.String(), nil
	}()
	if err != nil {
		return err
	}

	// We carry out two searches below, one attempting to find an issue that we
	// adopt (i.e. add a comment to) and one finding "related issues", i.e. those
	// that would match if it weren't for their branch label.
	qBase := fmt.Sprintf(
		`repo:%q user:%q is:issue is:open in:title label:%q sort:created-desc %q`,
		p.Repo, p.Org, searchLabel, title)

	releaseLabel := fmt.Sprintf("branch-%s", p.Branch)
	qExisting := qBase + " label:" + releaseLabel
	qRelated := qBase + " -label:" + releaseLabel

	rExisting, _, err := p.searchIssues(ctx, qExisting, &github.SearchOptions{
		ListOptions: github.ListOptions{
			PerPage: 1,
		},
	})
	if err != nil {
		// Tough luck, keep going even if that means we're going to add a duplicate
		// issue.
		//
		// TODO(tbg): surface this error.
		_ = err
		rExisting = &github.IssuesSearchResult{}
	}

	rRelated, _, err := p.searchIssues(ctx, qRelated, &github.SearchOptions{
		ListOptions: github.ListOptions{
			PerPage: 10,
		},
	})
	if err != nil {
		// This is no reason to throw the towel, keep going.
		//
		// TODO(tbg): surface this error.
		_ = err
		rRelated = &github.IssuesSearchResult{}
	}

	var foundIssue *int
	if len(rExisting.Issues) > 0 {
		// We found an existing issue to post a comment into.
		foundIssue = rExisting.Issues[0].Number
	}

	data.RelatedIssues = rRelated.Issues
	data.InternalLog = ctx.Builder.String()
	var body strings.Builder
	if err := bodyFn(&body, data); err != nil {
		// Failure is not an option.
		_ = err
		fmt.Fprintln(&body, "\nFailed to render body: "+err.Error())
	}

	createLabels := append(issueLabels, releaseLabel)
	createLabels = append(createLabels, req.ExtraLabels...)
	if foundIssue == nil {
		issueRequest := github.IssueRequest{
			Title:     &title,
			Body:      github.String(body.String()),
			Labels:    &createLabels,
			Assignee:  &assignee,
			Milestone: p.getProbableMilestone(ctx),
		}
		issue, _, err := p.createIssue(ctx, p.Org, p.Repo, &issueRequest)
		if err != nil {
			return errors.Wrapf(err, "failed to create GitHub issue %s",
				github.Stringify(issueRequest))
		}

		if req.ProjectColumnID != 0 {
			_, _, err := p.createProjectCard(ctx, int64(req.ProjectColumnID), &github.ProjectCardOptions{
				ContentID:   *issue.ID,
				ContentType: "Issue",
			})
			if err != nil {
				// Tough luck, keep going.
				//
				// TODO(tbg): retrieve the project column ID before posting, so that if
				// it can't be found we can mention that in the issue we'll file anyway.
				_ = err
			}
		}
	} else {
		comment := github.IssueComment{Body: github.String(body.String())}
		if _, _, err := p.createComment(
			ctx, p.Org, p.Repo, *foundIssue, &comment); err != nil {
			return errors.Wrapf(err, "failed to update issue #%d with %s",
				*foundIssue, github.Stringify(comment))
		}
	}

	return nil
}

func (p *poster) teamcityURL(tab, fragment string) *url.URL {
	options := url.Values{}
	options.Add("buildId", p.BuildID)
	options.Add("tab", tab)

	u, err := url.Parse(p.ServerURL)
	if err != nil {
		log.Fatal(err)
	}
	u.Scheme = "https"
	u.Path = "viewLog.html"
	u.RawQuery = options.Encode()
	u.Fragment = fragment
	return u
}

func (p *poster) teamcityBuildLogURL() *url.URL {
	return p.teamcityURL("buildLog", "")
}

func (p *poster) teamcityArtifactsURL(artifacts string) *url.URL {
	return p.teamcityURL("artifacts", artifacts)
}

func (p *poster) parameters() []string {
	var ps []string
	if p.Tags != "" {
		ps = append(ps, "TAGS="+p.Tags)
	}
	if p.Goflags != "" {
		ps = append(ps, "GOFLAGS="+p.Goflags)
	}
	return ps
}

// A PostRequest contains the information needed to create an issue about a
// test failure.
type PostRequest struct {
	// The title of the issue. See UnitTestFailureTitleTemplate for an example.
	TitleTemplate,
	// The body of the issue. See UnitTestFailureBodyTemplate for an example.
	BodyTemplate,
	// The name of the package the test failure relates to.
	PackageName,
	// The name of the failing test.
	TestName,
	// The test output, ideally shrunk to contain only relevant details.
	Message,
	// A link to the test artifacts. If empty, defaults to a link constructed
	// from the TeamCity env vars (if available).
	Artifacts,
	// The email of the author, used to determine which team/person to assign
	// the issue to.
	//
	// TODO(irfansharif): We should re-think this, and our general approach to
	// issue assignment, and move away from assigning individual authors.
	// #51653.
	AuthorEmail,
	// The instructions to reproduce the failure.
	ReproductionCommand string
	// Additional labels that will be added to the issue. They will be created
	// as necessary (as a side effect of creating an issue with them). An
	// existing issue may be adopted even if it does not have these labels.
	ExtraLabels []string

	// ProjectColumnID is the id of the GitHub project column to add the issue to,
	// or 0 if none.
	ProjectColumnID int
}

// Post either creates a new issue for a failed test, or posts a comment to an
// existing open issue. GITHUB_API_TOKEN must be set to a valid Github token
// that has permissions to search and create issues and comments or an error
// will be returned.
func Post(ctx context.Context, req PostRequest) error {
	opts := DefaultOptionsFromEnv()
	if !opts.CanPost() {
		return errors.Newf("GITHUB_API_TOKEN env variable is not set; cannot post issue")
	}

	client := github.NewClient(oauth2.NewClient(ctx, oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: opts.Token},
	)))
	return newPoster(client, opts).post(ctx, req, nil, nil)
}
