//go:build !darwin

package power

// platformStart is nil off a Mac: there is no assertion to take, and an
// [Assertion] built on nil counts holds and does nothing else.
var platformStart StartFunc
