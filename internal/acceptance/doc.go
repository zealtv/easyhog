// Package acceptance holds the MVP's end-to-end acceptance test. It has only
// test files, built on Unix, and they run only when EASYHOG_ACCEPTANCE=1 (see
// `make acceptance`): a whole reader's session takes about 40 seconds because
// real Groundhog runners tick once a second and consumers take real seconds.
//
// The test builds a temporary directory of synthetic repositories, each with a
// copy of the pinned upstream Groundhog script, and drives the real headless
// core (Refresher, Supervisor) and the Bubble Tea model through its Update
// method, with no terminal. It never touches this repository's own .groundhog/,
// a real schedule, the default tmux server or any model.
package acceptance
