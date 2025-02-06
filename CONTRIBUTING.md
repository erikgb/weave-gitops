# Contributing

Weave Gitops is a [Apache-2.0](LICENSE) project. This is an open source product with a community
led by volunteers interested in the brilliant software originally created by Weaveworks. :heart:

We welcome improvements to reporting issues and documentation as well as to code.

## Developer Certificate of Origin

By submitting any contributions to this repository as an individual or on behalf of a corporation, you agree to the [Developer Certificate of Origin](DCO).

## Understanding how to run development process

The [internal guide](doc/development-process.md) **is a work in progress** but aims to cover all aspects of how to
interact with the project and how to get involved in development as smoothly as possible.

## Acceptance Policy

These things will make a PR more likely to be accepted:

- a well-described requirement
- tests for new code
- tests for old code!
- new code and tests follow the conventions in old code and tests
- a good commit message (see below)
- all code must abide by [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments)
- names should abide by [What's in a name](https://talks.golang.org/2014/names.slide#1)
- code must build on both Linux and Darwin, via plain `go build`
- code should have appropriate test coverage and tests should be written
  to work with `go test`

In general, we will merge a PR once at least one maintainer has endorsed it. For substantial changes, more people may become involved, and you might get asked to resubmit the PR or divide the changes into more than one PR.

## Format of the Commit Message

Limit the subject to 50 characters and write as the continuation of the sentence "If applied, this commit will ..."
Explain what and why in the body, if more than a trivial change; wrap it at 72 characters.
The [following article](https://cbea.ms/git-commit/#seven-rules) has some more helpful advice on documenting your work.
