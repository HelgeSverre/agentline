package web

import _ "embed"

// IndexHTML is the embedded Agentline landing page.
//
//go:embed index.html
var IndexHTML []byte

// LLMSTXT is the concise machine-readable guide served at /llms.txt.
//
//go:embed llms.txt
var LLMSTXT []byte

// InspectHTML and InspectCSS are the embedded capability-scoped room viewer.
//
//go:embed inspect.html
var InspectHTML []byte

//go:embed inspect.css
var InspectCSS []byte

// InstallSH is the embedded Agentline installer.
//
//go:embed install.sh
var InstallSH []byte
