package version

// Version is set at build time via -ldflags "-X github.com/sgw/jailtime/pkg/version.Version=<tag>".
// It defaults to "dev" when not set.
var Version = "0.0.1"

const AppName = "jailtime"
