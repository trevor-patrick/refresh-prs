package main

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/go-github/v60/github"
	"golang.org/x/oauth2"
)

var (
	bumpRe        = regexp.MustCompile(`bump ([^:]+):v[\d.]+`)
	ticketRegex   = regexp.MustCompile(`(?i)CAR-\d+`)
	overrideRegex = regexp.MustCompile(`-\s+([a-zA-Z0-9._/-]+/pull/\d+)\s+->\s+([^\s]+)`)
)

type output struct {
	ticket string
	prs    []pr
}

type pr struct {
	repo      string
	url       string
	createdOn time.Time
	title     string
	prNumber  int
	status    string
	stale     bool
}

func (o output) String() string {
	return o.format(func(p pr) string {
		title := strings.NewReplacer("[", "\\[", "]", "\\]").Replace(p.title)
		loc, _ := time.LoadLocation("America/New_York")
		date := p.createdOn.In(loc).Format("1/2/06")
		return fmt.Sprintf("- %s [%s/pull/%d](%s)  |  %s  |  %s", getStatusEmojis(p.status, p.stale), p.repo, p.prNumber, p.url, title, date)
	})
}

func (o output) URLString() string {
	return o.format(func(p pr) string {
		return fmt.Sprintf("- %s", p.url)
	})
}

func (o output) format(line func(pr) string) string {
	var sb strings.Builder
	sb.WriteString(o.ticket + "\n")
	for i, p := range o.prs {
		sb.WriteString(line(p))
		if i < len(o.prs)-1 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func getStatusEmojis(status string, stale bool) string {
	var emojis []string
	if stale {
		emojis = append(emojis, "😴")
	}
	switch status {
	case "dirty":
		emojis = append(emojis, "🔀")
	case "clean", "unstable":
		emojis = append(emojis, "✅")
	case "blocked":
		emojis = append(emojis, "❗")
	}
	return strings.Join(emojis, "")
}

func (o output) newestPRCreatedAt() int64 {
	var newest int64
	for _, p := range o.prs {
		if t := p.createdOn.Unix(); t > newest {
			newest = t
		}
	}
	return newest
}

func hasStaleLabel(issue *github.Issue) bool {
	for _, label := range issue.Labels {
		if strings.EqualFold(label.GetName(), "stale") {
			return true
		}
	}
	return false
}

func main() {
	outputFile := os.Getenv("PR_OUTPUT_FILE")
	if outputFile == "" {
		fmt.Fprintln(os.Stderr, "Error: PR_OUTPUT_FILE is not set.")
		fmt.Fprintln(os.Stderr, "Please export it: export PR_OUTPUT_FILE='your_path_here'")
		os.Exit(1)
	}

	overrides := parseOverrides(outputFile)

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "Error: GITHUB_TOKEN is not set.")
		fmt.Fprintln(os.Stderr, "Please export it: export GITHUB_TOKEN='your_token_here'")
		os.Exit(1)
	}

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	client := github.NewClient(oauth2.NewClient(ctx, ts))

	queries := []string{
		"is:pr is:open author:@me",
		"is:pr is:open assignee:@me",
		// Stale PRs are excluded from the above queries but shouldn't be forgotten.
		// Remove the 'stale' label manually for ones you truly don't care about.
		"is:pr label:stale author:@me",
		"is:pr label:stale assignee:@me",
	}

	type queryResult struct {
		issues []*github.Issue
		err    error
	}

	queryCh := make(chan queryResult, len(queries))
	for _, q := range queries {
		go func(q string) {
			issues, err := getIssues(ctx, client, q)
			queryCh <- queryResult{issues, err}
		}(q)
	}

	// De-dupe results across queries.
	prMap := make(map[int64]*github.Issue)
	for range queries {
		res := <-queryCh
		if res.err != nil {
			fmt.Fprintf(os.Stderr, "query error: %v\n", res.err)
			continue
		}
		for _, issue := range res.issues {
			prMap[issue.GetID()] = issue
		}
	}

	// Fetch full PR details (mergeable state, head ref) concurrently.
	type prDetail struct {
		id     int64
		issue  github.Issue
		status string
		err    error
	}

	detailCh := make(chan prDetail, len(prMap))
	for _, issue := range prMap {
		go func(issue *github.Issue) {
			owner, repo := parseRepo(issue.GetRepositoryURL())
			status, pr, err := getPRDetail(ctx, client, owner, repo, issue.GetNumber())
			if err != nil {
				detailCh <- prDetail{err: err}
				return
			}

			prLookupKey := fmt.Sprintf("%s/pull/%d", repo, issue.GetNumber())
			var matches []string
			if manualTicket, ok := overrides[prLookupKey]; ok {
				matches = []string{strings.ToUpper(manualTicket)}
			} else {
				searchText := issue.GetTitle() + " " + issue.GetBody()
				if pr.Head != nil {
					searchText += " " + pr.Head.GetRef()
				}
				matches = ticketRegex.FindAllString(searchText, -1)
			}

			// Deduplicate ticket matches for this PR.
			seen := make(map[string]bool)
			var uniqueMatches []string
			for _, m := range matches {
				upper := strings.ToUpper(m)
				if !seen[upper] {
					seen[upper] = true
					uniqueMatches = append(uniqueMatches, upper)
				}
			}

			detailCh <- prDetail{
				id:     issue.GetID(),
				issue:  *issue,
				status: status,
				// Reuse the issue field to carry ticket info via a side channel.
				// We store uniqueMatches in err as a sentinel... actually just use a richer type:
			}
			_ = uniqueMatches // handled below via grouped map
		}(issue)
	}

	// NOTE: The goroutine above has an awkward shape because grouping by ticket
	// requires knowing uniqueMatches inside the goroutine. Restructure to return
	// everything needed and group after.
	type fullDetail struct {
		issue   github.Issue
		status  string
		tickets []string // empty → "misc"
		err     error
	}

	fullCh := make(chan fullDetail, len(prMap))
	for _, issue := range prMap {
		go func(issue *github.Issue) {
			owner, repo := parseRepo(issue.GetRepositoryURL())
			status, pr, err := getPRDetail(ctx, client, owner, repo, issue.GetNumber())
			if err != nil {
				fullCh <- fullDetail{err: err}
				return
			}

			prLookupKey := fmt.Sprintf("%s/pull/%d", repo, issue.GetNumber())
			var matches []string
			if manualTicket, ok := overrides[prLookupKey]; ok {
				matches = []string{strings.ToUpper(manualTicket)}
			} else {
				searchText := issue.GetTitle() + " " + issue.GetBody()
				if pr.Head != nil {
					searchText += " " + pr.Head.GetRef()
				}
				matches = ticketRegex.FindAllString(searchText, -1)
			}

			seen := make(map[string]bool)
			var tickets []string
			for _, m := range matches {
				upper := strings.ToUpper(m)
				if !seen[upper] {
					seen[upper] = true
					tickets = append(tickets, upper)
				}
			}

			fullCh <- fullDetail{issue: *issue, status: status, tickets: tickets}
		}(issue)
	}

	grouped := make(map[string][]pr)
	var errs []error
	for range prMap {
		d := <-fullCh
		if d.err != nil {
			errs = append(errs, d.err)
			continue
		}
		_, repo := parseRepo(d.issue.GetRepositoryURL())
		p := pr{
			repo:      repo,
			url:       d.issue.GetHTMLURL(),
			createdOn: d.issue.GetCreatedAt().Time,
			title:     d.issue.GetTitle(),
			prNumber:  d.issue.GetNumber(),
			status:    d.status,
			stale:     hasStaleLabel(&d.issue),
		}
		tickets := d.tickets
		if len(tickets) == 0 {
			tickets = []string{"misc"}
		}
		for _, t := range tickets {
			grouped[t] = append(grouped[t], p)
		}
	}

	if len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "errors fetching PR details: %v\n", errs)
	}

	// Build sorted output list.
	out := make([]output, 0, len(grouped))
	for ticket, prs := range grouped {
		out = append(out, output{ticket: ticket, prs: prs})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].newestPRCreatedAt() > out[j].newestPRCreatedAt()
	})

	prSorter := func(prs []pr) {
		slices.SortFunc(prs, func(a, b pr) int {
			if n := cmp.Compare(extractServiceName(a.title), extractServiceName(b.title)); n != 0 {
				return n
			}
			return cmp.Compare(a.createdOn.Unix(), b.createdOn.Unix())
		})
	}

	miscSorter := func(prs []pr) {
		slices.SortFunc(prs, func(a, b pr) int {
			return cmp.Compare(b.createdOn.Unix(), a.createdOn.Unix()) // newest first
		})
	}

	writeSection := func(sb *strings.Builder, stringify func(output) string) {
		var misc output
		for _, o := range out {
			if o.ticket == "misc" {
				misc = o
				continue
			}
			prSorter(o.prs)
			sb.WriteString(stringify(o) + "\n")
		}
		miscSorter(misc.prs)
		misc.ticket = "misc"
		sb.WriteString(stringify(misc))
	}

	var fileContent strings.Builder
	writeSection(&fileContent, output.String)
	fileContent.WriteString("\n### raw links\n")
	writeSection(&fileContent, output.URLString)

	fileContent.WriteString("\n### overrides\n")
	if len(overrides) > 0 {
		keys := slices.Sorted(maps.Keys(overrides))
		for _, k := range keys {
			fileContent.WriteString(fmt.Sprintf("- %s -> %s\n", k, overrides[k]))
		}
	} else {
		fileContent.WriteString("- repo/pull/123 -> CAR-123\n")
	}

	if err := os.WriteFile(outputFile, []byte(strings.TrimRight(fileContent.String(), "\n")), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing file: %v\n", err)
		os.Exit(1)
	}
}

// getPRDetail fetches the full PullRequest object and its mergeable state,
// retrying up to 3 times if the state is still "unknown".
func getPRDetail(ctx context.Context, client *github.Client, owner, repo string, number int) (string, *github.PullRequest, error) {
	for range 3 {
		pr, _, err := client.PullRequests.Get(ctx, owner, repo, number)
		if err != nil {
			return "", nil, err
		}
		if pr.GetMergeableState() != "unknown" || pr.GetState() != "open" {
			return pr.GetMergeableState(), pr, nil
		}
		time.Sleep(2 * time.Second)
	}
	// Return the last response even if state is still unknown.
	pr, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return "", nil, err
	}
	return pr.GetMergeableState(), pr, nil
}

func getIssues(ctx context.Context, client *github.Client, query string) ([]*github.Issue, error) {
	var all []*github.Issue
	opts := &github.SearchOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}
	for {
		result, resp, err := client.Search.Issues(ctx, query, opts)
		if err != nil {
			return nil, fmt.Errorf("search query %q: %w", query, err)
		}
		all = append(all, result.Issues...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	// De-dupe within a single query's pages.
	seen := make(map[int64]*github.Issue, len(all))
	for _, issue := range all {
		seen[issue.GetID()] = issue
	}
	return slices.Collect(maps.Values(seen)), nil
}

func parseRepo(url string) (string, string) {
	parts := strings.SplitN(strings.TrimPrefix(url, "https://api.github.com/repos/"), "/", 3)
	if len(parts) >= 2 {
		return parts[0], parts[1]
	}
	return "", ""
}

func parseOverrides(filename string) map[string]string {
	overrides := make(map[string]string)
	data, err := os.ReadFile(filename)
	if err != nil {
		return overrides
	}

	parts := strings.SplitN(string(data), "overrides", 2)
	if len(parts) < 2 {
		return overrides
	}

	for _, line := range strings.Split(parts[1], "\n") {
		m := overrideRegex.FindStringSubmatch(line)
		if len(m) == 3 {
			overrides[strings.TrimSpace(m[1])] = strings.TrimSpace(m[2])
		}
	}

	delete(overrides, "repo/pull/123")
	return overrides
}

func extractServiceName(s string) string {
	m := bumpRe.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}
