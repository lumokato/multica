package agent

// piCancelHelperEnv selects the fake-Pi cancellation helper in TestMain. It is
// declared without a build tag because TestMain is shared by every platform;
// the helper implementation itself is POSIX-only.
const piCancelHelperEnv = "MULTICA_PI_CANCEL_HELPER"
