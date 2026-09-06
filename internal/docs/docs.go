// Package docs holds the one string every refusal sends a reader to.
//
// It is a package of its own because both surfaces need it and neither should
// import the other: internal/ledgerhttp puts it in a refusal body, cmd/pacioli
// puts it on stderr.
package docs

// Home is where a refusal sends someone who wants the rule behind it.
//
// There is no documentation site yet, so this names only what a reader can
// actually reach today: the repository and the binary's own help. A published
// site was floated as docs.pacioli.dev; when it exists, this constant is the
// only line in the repository that changes and every refusal follows it.
const Home = "github.com/mgballou/pacioli — README.md and docs/DESIGN.md, or run `pacioli --help`"
