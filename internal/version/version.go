// Package version porte la version injectée au build (-ldflags -X).
package version

// Version est remplacée au build par le tag Git ou le SHA.
var Version = "dev"
