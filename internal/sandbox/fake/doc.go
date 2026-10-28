// Package fake is the CI sandbox backend: it runs commands in a temporary
// directory on the host with a scrubbed environment and no isolation. It is
// compiled only under build tag fakesandbox and additionally refuses to run
// unless the environment variable MYCEL_FAKE_SANDBOX_OK is "1", so a binary
// built with the tag by mistake still cannot use it (spec: "It refuses to
// start outside test builds").
//
// What it does enforce, so job logic can be tested: /workspace emulation
// (cwd = <Dir>/workspace, seeded by copy), env scrubbing (only Spec.Env plus
// PATH, HOME=<Dir>/home, TMPDIR), HTTP_PROXY/HTTPS_PROXY/NO_PROXY pointing at
// the egress proxy, wall clock, max output size, CopyIn/CopyOut/Glob,
// Pause/Resume via SIGSTOP/SIGCONT on unix, Destroy removing the directory.
// It does not enforce memory, CPU or process limits and reports Isolation
// "none".
//
// Notes on the implementation: the wall clock and the log cap are enforced
// by sandbox.Runner, not here. Capabilities reports Egress true so the fake
// profile's allowlist is accepted, but the proxy variables are only set, not
// enforced. On unix every command leads its own process group and
// TERM/KILL/Destroy signal the whole group; on windows only the leader is
// killed (a Job Object to kill the whole tree will come with the daemon's
// windows work).
package fake
