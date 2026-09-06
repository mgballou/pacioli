// Package docs holds the one string every refusal sends a reader to.
//
// It is a package of its own because both surfaces need it and neither should
// import the other.
package docs

// Home is where a refusal sends someone who wants the rule behind it.
//
// There is no documentation site yet, so this names only what a reader can
// reach today. When there is one, this constant is the only line that changes.
const Home = "github.com/mgballou/pacioli — README.md and docs/DESIGN.md, or run `pacioli --help`"
