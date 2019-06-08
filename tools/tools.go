// +build tools

package tools

import (
	// Pin versions of these tools by having an unused import
	_ "github.com/kisielk/errcheck"
	_ "golang.org/x/lint/golint"
	_ "honnef.co/go/tools/cmd/staticcheck"
)
