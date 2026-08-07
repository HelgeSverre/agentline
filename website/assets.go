package website

import _ "embed"

// IndexHTML is the embedded Agentline landing page.
//
//go:embed index.html
var IndexHTML []byte
