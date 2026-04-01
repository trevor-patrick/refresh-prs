# PR Tracker
A CLI tool that fetches your open GitHub pull requests, groups them by Linear ticket, and writes a formatted Markdown summary.
## What it does
- Searches GitHub for all open PRs where you are the **author** or **assignee**, including any with a `stale` label so nothing falls through the cracks. To stop tracking a stale PR, remove its `stale` label on GitHub
- Groups PRs by Linear ticket (e.g. `CAR-1234`), detected from the PR title, body, or branch name
- Fetches each PR's status and displays it as an emoji
- Sorts groups by most recently created PR, and sorts PRs within a group by service name
- Writes the result to `pull requests.md`, including a `misc` section for PRs with no detectable ticket
- Supports **manual overrides** — if a PR can't be automatically matched to a ticket, you can assign it by adding a line like `- some-service/pull/123 -> CAR-456` to the `overrides` section of the Markdown file. On the next run, the tool reads these overrides and groups those PRs accordingly. The placeholder line `- repo/pull/123 -> CAR-123` is ignored and is just an example
- The Markdown file also includes a `raw links` section with bare URLs for easy copy/pasting into a Slack
## Output format
Each ticket group looks like this:

```
CAR-123
- ✅ some-service [some-service/pull/42](https://github.com/...) | bump some-service:v1.2.3 | 3/15/25
- ❗ other-service [other-service/pull/99](https://github.com/...) | fix: something | 3/14/25
```
### Status emojis

| Emoji | Meaning                                 |
| ----- | --------------------------------------- |
| ✅     | Ready to merge                          |
| 🔀    | Merge conflict                          |
| ❗     | Blocked — failing checks or other issue |
| 😴    | PR is stale                             |

## Setup

### Prerequisites

- Go 1.22+
- A GitHub personal access token with `repo` scope
### Environment

```bash
export GITHUB_TOKEN="your_token_here"
export PR_OUTPUT_FILE="your_path_here"
```

## Usage

```bash
go run refresh_prs.go
```
