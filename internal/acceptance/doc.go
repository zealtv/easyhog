// Package acceptance holds Easyhog's end-to-end acceptance test. It has only
// test files, built on Unix, and they run only when EASYHOG_ACCEPTANCE=1 (see
// `make acceptance`): a whole reader's session takes most of a minute, because
// real ticks come once a second and runs take real time.
//
// The test builds a temporary directory of synthetic repositories, each with a
// copy of the pinned upstream Groundhog script and items whose `run` comes from
// examples/runs, and drives the real headless core (Refresher, Ticker) and the
// Bubble Tea model through its Update method, with no terminal. It never touches
// this repository's own .groundhog/, a real schedule, the default tmux server or
// any model.
package acceptance
