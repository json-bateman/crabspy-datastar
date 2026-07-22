package web

import "runtime/debug"

// sets Commit at build time using -ldflags
//
//	go build -ldflags "-X 'crabspy/web.Commit=$(git rev-parse --short HEAD)'"
var Commit string

// CommitLabel returns a short git revision for display, or "dev" if unknown.
func CommitLabel() string {
	if Commit != "" {
		return shortRev(Commit)
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return shortRev(s.Value)
			}
		}
	}
	return "dev"
}

func shortRev(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}
